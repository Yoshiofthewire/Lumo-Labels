package lumo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type warmupState struct {
	mu       sync.Mutex
	ready    bool
	inFlight chan struct{}
}

var (
	warmupStatesMu sync.Mutex
	warmupStates   = map[string]*warmupState{}
)

const warmupInitialDelay = 15 * time.Second
const warmupRequestTimeout = 3 * time.Minute
const warmupRetryDelay = 10 * time.Second
const warmupMaxAttempts = 3
const warmupReadinessPollInterval = 3 * time.Second
const warmupReadySettle = 3 * time.Second

// Classify pacing/serialization. The Lumo model handles one generation at a
// time; firing concurrent or back-to-back requests triggers "Model busy" and
// empty-message responses. These constants serialize and space out calls.
const (
	// classifyPaceInterval is the minimum gap enforced between the end of one
	// classify request and the start of the next.
	classifyPaceInterval = 3 * time.Second
	// classifyFirstBackoff is the short backoff used before the first retry of a
	// transient (empty-message / tools-only) response.
	classifyFirstBackoff = 2 * time.Second
	// classifyRetryBackoff is the backoff used for retries after the first.
	classifyRetryBackoff = 5 * time.Second
)

type HTTPClient struct {
	baseURL   string
	apiKey    string
	path      string
	guardrail string
	tuning    string
	client    *http.Client

	// classifyMu serializes classify requests so only one generation hits the
	// Lumo model at a time. It is held for the full duration of a Classify call
	// (including retries and pacing waits).
	classifyMu sync.Mutex
	// lastClassify records when the most recent classify request finished; it is
	// guarded by classifyMu and used to pace consecutive calls.
	lastClassify time.Time
}

func (c *HTTPClient) Warmup(ctx context.Context) error {
	return c.ensureWarm(ctx)
}

func NewHTTPClient(baseURL, apiKey, path, guardrail, tuning string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &HTTPClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		path:      path,
		guardrail: strings.TrimSpace(guardrail),
		tuning:    strings.TrimSpace(tuning),
		client:    &http.Client{Timeout: timeout},
	}
}

func (c *HTTPClient) Classify(ctx context.Context, allowedLabels []string, sender, subject, body string) (string, error) {
	if err := c.ensureWarm(ctx); err != nil {
		appendLumoErrorLog(err.Error())
		return "", err
	}

	// Serialize: only one classify generation may hit the Lumo model at a time.
	// The lock is held across retries and pacing so concurrent callers (the
	// poller and the /api test-classify endpoint) queue instead of overlapping.
	c.classifyMu.Lock()
	defer c.classifyMu.Unlock()

	// Pace: enforce a minimum gap since the previous classify finished so the
	// model is not hammered back-to-back.
	if err := c.paceClassify(ctx); err != nil {
		return "", err
	}
	defer func() { c.lastClassify = time.Now() }()

	appendLumoServerLog(fmt.Sprintf("[CLASSIFY] From: %s | Subject: [%s]", sender, subject))

	prompt := buildRuntimePrompt(allowedLabels, sender, subject, body)
	payload := []byte(fmt.Sprintf("{\"prompt\":%s, \"webSearch\":false}", strconv.Quote(prompt)))

	for attempt := 0; attempt < 3; attempt++ {
		result, err := c.classifyOnce(ctx, payload)
		if err != nil {
			appendLumoErrorLog(err.Error())
			return "", err
		}
		appendLumoOutputLog(result)

		normalized := strings.TrimSpace(result)
		if strings.EqualFold(normalized, "Error: Model busy") {
			_ = restartLumoServer(ctx)
			return "", fmt.Errorf("lumo model busy; restarted lumo server")
		}
		if strings.Contains(result, "You've reached your weekly chat limit") {
			return "", fmt.Errorf("%s\nuser has run out of ai credits", normalized)
		}
		if isToolsOnlyResponse(normalized) {
			appendLumoServerLog(fmt.Sprintf("[CLASSIFY RETRY] tools-only response on attempt %d/%d, waiting before retry", attempt+1, 3))
			if attempt < 2 {
				if err := sleepWithContext(ctx, classifyRetryDelay(attempt, 15*time.Second)); err != nil {
					return "", err
				}
				continue
			}
			appendLumoServerLog("[CLASSIFY FAILED] tools-only response exhausted all inner retries")
			return "", fmt.Errorf("lumo returned tools-only response after %d attempts", attempt+1)
		}
		if hasEmptyMessageNoise(normalized) {
			appendLumoServerLog(fmt.Sprintf("[CLASSIFY RETRY] empty-message noise on attempt %d/%d, waiting before retry", attempt+1, 3))
			if attempt < 2 {
				if err := sleepWithContext(ctx, classifyRetryDelay(attempt, classifyRetryBackoff)); err != nil {
					return "", err
				}
				continue
			}
			appendLumoServerLog("[CLASSIFY FAILED] empty-message noise exhausted all inner retries")
			return "", fmt.Errorf("lumo returned empty-message noise after %d attempts", attempt+1)
		}

		searchText := stripTransientNoise(labelSearchScope(normalized))
		appendLumoServerLog(fmt.Sprintf("[CLASSIFY RESPONSE] %s", strings.SplitN(searchText, "\n", 2)[0]))

		// Find the first line that matches an allowed label (case-insensitive)
		for _, line := range strings.Split(searchText, "\n") {
			line = strings.TrimSpace(line)
			for _, label := range allowedLabels {
				if strings.EqualFold(line, label) {
					return label, nil
				}
			}
		}
		// No exact match — return the last non-empty line as best effort
		lines := strings.Split(searchText, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return l, nil
			}
		}
		return normalized, nil
	}

	return "", fmt.Errorf("lumo classify retry limit reached")
}

// paceClassify blocks until classifyPaceInterval has elapsed since the previous
// classify request finished, so consecutive calls are spaced out. It must be
// called while holding classifyMu. Returns the context error if cancelled while
// waiting.
func (c *HTTPClient) paceClassify(ctx context.Context) error {
	if classifyPaceInterval <= 0 || c.lastClassify.IsZero() {
		return nil
	}
	wait := classifyPaceInterval - time.Since(c.lastClassify)
	if wait <= 0 {
		return nil
	}
	return sleepWithContext(ctx, wait)
}

// classifyRetryDelay returns the backoff before a retry. The first retry uses a
// short backoff so a single transient response recovers quickly; later retries
// use the supplied (longer) delay.
func classifyRetryDelay(attempt int, subsequent time.Duration) time.Duration {
	if attempt == 0 {
		return classifyFirstBackoff
	}
	return subsequent
}

// sleepWithContext sleeps for d unless the context is cancelled first, in which
// case it returns the context error.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func buildRuntimePrompt(allowedLabels []string, sender, subject, body string) string {
	body = strings.TrimSpace(body)
	sender = strings.TrimSpace(sender)
	subject = strings.TrimSpace(subject)

	parts := make([]string, 0, 5)
	if len(allowedLabels) > 0 {
		parts = append(parts, "Classify this email. Reply with exactly one label from: "+strings.Join(allowedLabels, ", ")+". Reply with only the label name and nothing else.")
		parts = append(parts, "")
	}
	if sender != "" {
		parts = append(parts, "Email Address: "+sender)
	}
	if subject != "" {
		parts = append(parts, "Subject Line: "+subject)
	}
	if body != "" {
		parts = append(parts, body)
	}
	return strings.Join(parts, "\n")
}

// ParseAllowedLabels extracts the bullet-list items under the "## Allowed Labels" heading from a TUNING.md document.
func ParseAllowedLabels(text string) []string {
	var labels []string
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## Allowed Labels") {
			inSection = true
			continue
		}
		if inSection {
			if strings.HasPrefix(trimmed, "## ") {
				break
			}
			if strings.HasPrefix(trimmed, "- ") {
				if label := strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")); label != "" {
					labels = append(labels, label)
				}
			}
		}
	}
	return labels
}

func (c *HTTPClient) ensureWarm(ctx context.Context) error {
	state := getWarmupState(c.baseURL + c.path)

	for {
		state.mu.Lock()
		if state.ready {
			state.mu.Unlock()
			return nil
		}
		if state.inFlight == nil {
			state.inFlight = make(chan struct{})
			state.mu.Unlock()
			err := c.runWarmup(ctx)

			state.mu.Lock()
			if err == nil {
				state.ready = true
			}
			close(state.inFlight)
			state.inFlight = nil
			state.mu.Unlock()
			return err
		}
		inFlight := state.inFlight
		state.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-inFlight:
		}
	}
}

func (c *HTTPClient) runWarmup(ctx context.Context) error {
	// Wait for the Lumo API to start accepting connections before sending any
	// warmup documents. On container restart the Lumo process (Firefox via
	// Playwright on port 3333) can take well over a minute to begin listening,
	// during which connection attempts are refused. Treat that as "not ready
	// yet" rather than a warmup failure.
	if err := c.waitForLumoReady(ctx); err != nil {
		return err
	}

	guardrail := LoadGuardrailText()
	if guardrail == "" {
		guardrail = strings.TrimSpace(c.guardrail)
	}
	if guardrail != "" {
		if err := c.sendWarmupDocument(ctx, "guardrail", guardrail); err != nil {
			return err
		}
	}

	tuning := LoadTuningText()
	if tuning == "" {
		tuning = strings.TrimSpace(c.tuning)
	}
	if tuning != "" {
		if err := c.sendWarmupDocument(ctx, "tuning", tuning); err != nil {
			return err
		}
	}

	return nil
}

// waitForLumoReady blocks until the Lumo API accepts a TCP connection or the
// context is cancelled. This absorbs the startup window where the Lumo process
// is still launching and refuses connections, so warmup does not fail spuriously
// with "connection refused".
func (c *HTTPClient) waitForLumoReady(ctx context.Context) error {
	addr := hostPortFromURL(c.baseURL)
	if addr == "" {
		// Could not determine an address to probe; fall back to the original
		// fixed delay so behaviour is unchanged for unusual base URLs.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(warmupInitialDelay):
		}
		return nil
	}

	logged := false
	for {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			_ = conn.Close()
			// The port is open; give the server a brief moment to finish
			// initializing before sending the first document.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(warmupReadySettle):
			}
			return nil
		}

		if !logged {
			appendLumoServerLog(fmt.Sprintf("[LUMO WARMUP] waiting for Lumo API at %s to accept connections", addr))
			logged = true
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("lumo not ready: %w", ctx.Err())
		case <-time.After(warmupReadinessPollInterval):
		}
	}
}

// hostPortFromURL extracts a dialable host:port from a base URL, applying the
// default port for the scheme when none is specified.
func hostPortFromURL(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	port := "80"
	if strings.EqualFold(u.Scheme, "https") {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func (c *HTTPClient) sendWarmupDocument(ctx context.Context, name, content string) error {
	prompt := buildWarmupPrompt(name, content)
	payload := []byte(fmt.Sprintf("{\"prompt\":%s, \"webSearch\":false}", strconv.Quote(prompt)))
	upper := strings.ToUpper(name)
	appendLumoServerLog(fmt.Sprintf("[LUMO WARMUP %s] sending prompt", upper))
	var lastErr error
	for attempt := 0; attempt < warmupMaxAttempts; attempt++ {
		started := time.Now()
		result, err := c.classifyOnceWithTimeout(ctx, payload, warmupRequestTimeout, false)
		elapsed := time.Since(started).Round(time.Second)
		if err == nil {
			if !isThoughtAck(result) {
				preview := firstLine(strings.TrimSpace(result))
				lastErr = fmt.Errorf("lumo %s warmup failed: expected 'Thought about this' acknowledgement, got %q", name, strings.TrimSpace(result))
				appendLumoServerLog(fmt.Sprintf("[LUMO WARMUP %s] attempt %d/%d unexpected response after %s: %q", upper, attempt+1, warmupMaxAttempts, elapsed, preview))
			} else {
				appendLumoServerLog(fmt.Sprintf("[LUMO WARMUP %s] acknowledged after %s", upper, elapsed))
				return nil
			}
		} else {
			lastErr = fmt.Errorf("lumo %s warmup failed: %w", name, err)
			appendLumoServerLog(fmt.Sprintf("[LUMO WARMUP %s] attempt %d/%d failed after %s: %s", upper, attempt+1, warmupMaxAttempts, elapsed, err.Error()))
		}

		if attempt < warmupMaxAttempts-1 {
			appendLumoServerLog(fmt.Sprintf("[LUMO WARMUP %s] retrying in %s", upper, warmupRetryDelay))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(warmupRetryDelay):
			}
		}
	}
	return lastErr
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func buildWarmupPrompt(name, content string) string {
	trimmed := strings.TrimSpace(content)
	upper := strings.ToUpper(strings.TrimSpace(name))
	return fmt.Sprintf(
		"You are performing startup preparation for %s. Read the entire document between START and FINISH before responding. Do not summarize, do not echo content, and do not call tools. After you have fully read all lines, acknowledge by replying with exactly: Thought about this\n\nSTART %s\n%s\nFINISH %s",
		upper,
		upper,
		trimmed,
		upper,
	)
}

func labelSearchScope(result string) string {
	lines := strings.Split(strings.TrimSpace(result), "\n")
	start := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.Trim(lines[i], "`"))
		if strings.EqualFold(line, "Thought about this") {
			start = i + 1
			break
		}
	}
	if start >= len(lines) {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n"))
}

func getWarmupState(key string) *warmupState {
	warmupStatesMu.Lock()
	defer warmupStatesMu.Unlock()

	state, ok := warmupStates[key]
	if ok {
		return state
	}
	state = &warmupState{}
	warmupStates[key] = state
	return state
}

func ResetWarmupState() {
	warmupStatesMu.Lock()
	defer warmupStatesMu.Unlock()
	for _, state := range warmupStates {
		state.mu.Lock()
		state.ready = false
		state.mu.Unlock()
	}
}

func isThoughtAck(s string) bool {
	normalized := strings.TrimSpace(strings.Trim(s, "`"))
	if strings.EqualFold(normalized, "Thought about this") {
		return true
	}
	for _, line := range strings.Split(normalized, "\n") {
		if strings.EqualFold(strings.TrimSpace(strings.Trim(line, "`")), "Thought about this") {
			return true
		}
	}
	return false
}

func (c *HTTPClient) classifyOnce(ctx context.Context, payload []byte) (string, error) {
	return c.classifyOnceWithTimeout(ctx, payload, c.client.Timeout, true)
}

func (c *HTTPClient) classifyOnceWithTimeout(ctx context.Context, payload []byte, timeout time.Duration, logInput bool) (string, error) {
	if logInput {
		appendLumoInputLog(extractPromptFromPayload(payload))
	}
	client := c.client
	if timeout > 0 && c.client.Timeout != timeout {
		copyClient := *c.client
		copyClient.Timeout = timeout
		client = &copyClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.path, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		body := strings.TrimSpace(string(bodyBytes))
		if body != "" {
			return "", fmt.Errorf("lumo classify failed: status %d body: %s", resp.StatusCode, body)
		}
		return "", fmt.Errorf("lumo classify failed: status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var parsed map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return "", err
		}
		for _, key := range []string{"label", "text", "response", "output", "message", "error"} {
			if v, ok := parsed[key]; ok {
				if s, ok := v.(string); ok {
					return strings.TrimSpace(s), nil
				}
			}
		}
		return "", fmt.Errorf("lumo response missing text field")
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(rawBytes))
	if raw == "" {
		return "", fmt.Errorf("lumo returned empty response")
	}
	return raw, nil
}

func extractPromptFromPayload(payload []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return strings.TrimSpace(string(payload))
	}
	v, ok := parsed["prompt"]
	if !ok {
		return strings.TrimSpace(string(payload))
	}
	s, ok := v.(string)
	if !ok {
		return strings.TrimSpace(string(payload))
	}
	return strings.TrimSpace(s)
}

func appendLumoInputLog(message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/lumo_lab/logs"
	}
	path := filepath.Join(logDir, "lumo.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	ts := time.Now().Format("2006-01-02 15:04:05")
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_, _ = fmt.Fprintf(f, "[%s] [Lumo Input] %s\n", ts, line)
	}
}

func appendLumoOutputLog(result string) {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/lumo_lab/logs"
	}
	path := filepath.Join(logDir, "lumo.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	ts := time.Now().Format("2006-01-02 15:04:05")
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_, _ = fmt.Fprintf(f, "[%s] [LUMO OUTPUT] %s\n", ts, line)
	}
}

func appendLumoServerLog(message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/lumo_lab/logs"
	}
	path := filepath.Join(logDir, "lumo-server.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().Format("2006-01-02 15:04:05")
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_, _ = fmt.Fprintf(f, "[%s] %s\n", ts, line)
	}
}

func appendLumoErrorLog(message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/lumo_lab/logs"
	}
	path := filepath.Join(logDir, "lumo.err.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	ts := time.Now().Format("2006-01-02 15:04:05")
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_, _ = fmt.Fprintf(f, "[%s] [LUMO ERROR] %s\n", ts, line)
	}
}

func restartLumoServer(ctx context.Context) error {
	restartCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(restartCtx, "supervisorctl", "-c", "/etc/supervisord.conf", "restart", "lumo")
	if err := cmd.Run(); err != nil {
		return err
	}
	ResetWarmupState()
	return nil
}

func isToolsOnlyResponse(s string) bool {
	normalized := strings.TrimSpace(s)
	if normalized == "" {
		return false
	}

	// Handle common markdown wrappers around a one-word reply.
	for {
		before := normalized
		normalized = strings.TrimSpace(strings.Trim(normalized, "`"))
		if strings.HasPrefix(normalized, "```") && strings.HasSuffix(normalized, "```") {
			normalized = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(normalized, "```"), "```"))
		}
		if normalized == before {
			break
		}
	}

	return strings.EqualFold(normalized, "Tools")
}

func hasEmptyMessageNoise(s string) bool {
	return strings.Contains(strings.ToLower(s), "this message is empty. sorry about that")
}

func stripTransientNoise(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		lower := strings.ToLower(l)
		if lower == "this message is empty. sorry about that." {
			continue
		}
		clean = append(clean, l)
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}

func LoadGuardrailText() string {
	paths := []string{}
	if envPath := strings.TrimSpace(os.Getenv("GARDRAIL_FILE")); envPath != "" {
		paths = append(paths, envPath)
	}
	paths = append(paths, "GARDRAIL.md", "/opt/lumo-lab/GARDRAIL.md")

	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(b))
		if text != "" {
			return text
		}
	}
	return ""
}

func LoadTuningText() string {
	paths := []string{}
	if envPath := strings.TrimSpace(os.Getenv("TUNING_FILE")); envPath != "" {
		paths = append(paths, envPath)
	}
	paths = append(paths, "/lumo_lab/config/TUNING.md", "TUNING.md", "/opt/lumo-lab/TUNING.md")

	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(b))
		if text != "" {
			return text
		}
	}
	return ""
}
