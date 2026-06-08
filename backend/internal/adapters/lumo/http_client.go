package lumo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

type HTTPClient struct {
	baseURL   string
	apiKey    string
	path      string
	guardrail string
	tuning    string
	client    *http.Client
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

	prompt := buildRuntimePrompt(sender, subject, body)
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
			if attempt < 2 {
				time.Sleep(15 * time.Second)
				continue
			}
			return "", fmt.Errorf("lumo returned tools-only response after retries")
		}
		if hasEmptyMessageNoise(normalized) {
			if attempt < 2 {
				time.Sleep(5 * time.Second)
				continue
			}
		}

		searchText := stripTransientNoise(labelSearchScope(normalized))

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

func buildRuntimePrompt(sender, subject, body string) string {
	body = strings.TrimSpace(body)
	sender = strings.TrimSpace(sender)
	subject = strings.TrimSpace(subject)

	if sender == "" && subject == "" {
		return body
	}

	parts := make([]string, 0, 3)
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
	timer := time.NewTimer(warmupInitialDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
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

func (c *HTTPClient) sendWarmupDocument(ctx context.Context, name, content string) error {
	prompt := buildWarmupPrompt(name, content)
	payload := []byte(fmt.Sprintf("{\"prompt\":%s, \"webSearch\":false}", strconv.Quote(prompt)))
	appendLumoOutputLog(fmt.Sprintf("[LUMO WARMUP %s] prompt sent", strings.ToUpper(name)))
	var lastErr error
	for attempt := 0; attempt < warmupMaxAttempts; attempt++ {
		result, err := c.classifyOnceWithTimeout(ctx, payload, warmupRequestTimeout, false)
		if err == nil {
			if !isThoughtAck(result) {
				lastErr = fmt.Errorf("lumo %s warmup failed: expected 'Thought about this' acknowledgement, got %q", name, strings.TrimSpace(result))
			} else {
				appendLumoOutputLog(fmt.Sprintf("[LUMO WARMUP %s] Thought about this", strings.ToUpper(name)))
				return nil
			}
		} else {
			lastErr = fmt.Errorf("lumo %s warmup failed: %w", name, err)
		}

		if attempt < warmupMaxAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(warmupRetryDelay):
			}
		}
	}
	return lastErr
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
