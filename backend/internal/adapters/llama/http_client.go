package llama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const warmupRequestTimeout = 3 * time.Minute

// Classify pacing/serialization. The model handles one generation at a time;
// firing concurrent or back-to-back requests can increase latency and errors.
const (
	classifyPaceInterval = 3 * time.Second
	classifyFirstBackoff = 2 * time.Second
	classifyRetryBackoff = 5 * time.Second
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

// ResetWarmupState clears cached warmup readiness so the next classify/warmup
// re-runs model pull/readiness initialization.
func ResetWarmupState() {
	warmupStatesMu.Lock()
	defer warmupStatesMu.Unlock()
	warmupStates = map[string]*warmupState{}
}

type HTTPClient struct {
	baseURL string
	path    string
	model   string
	client  *http.Client

	tuningTemplate string

	classifyMu   sync.Mutex
	lastClassify time.Time
}

func NewHTTPClient(baseURL, _apiKey, path, tuning string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if strings.TrimSpace(path) == "" {
		path = "/api/generate"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	model := strings.TrimSpace(os.Getenv("OLLAMA_MODEL"))
	if model == "" {
		model = "qwen3:1.7b"
	}

	tuningTemplate := strings.TrimSpace(tuning)

	return &HTTPClient{
		baseURL:        strings.TrimRight(baseURL, "/"),
		path:           path,
		model:          model,
		client:         &http.Client{Timeout: timeout},
		tuningTemplate: tuningTemplate,
	}
}

func (c *HTTPClient) Warmup(ctx context.Context) error {
	return c.ensureWarm(ctx)
}

func (c *HTTPClient) Classify(ctx context.Context, allowedLabels []string, sender, subject, body string) (string, error) {
	if err := c.ensureWarm(ctx); err != nil {
		appendLlamaErrorLog(err.Error())
		return "", err
	}

	c.classifyMu.Lock()
	defer c.classifyMu.Unlock()

	if err := c.paceClassify(ctx); err != nil {
		return "", err
	}
	defer func() { c.lastClassify = time.Now() }()

	appendLlamaServerLog(fmt.Sprintf("[CLASSIFY] From: %s | Subject: [%s]", sender, subject))

	prompt := buildRuntimePrompt(c.tuningTemplate, allowedLabels, sender, subject, body)
	for attempt := 0; attempt < 3; attempt++ {
		result, err := c.classifyOnce(ctx, prompt)
		if err != nil {
			appendLlamaErrorLog(err.Error())
			return "", err
		}

		normalized := strings.TrimSpace(result)
		appendLlamaOutputLog(normalized)

		if strings.Contains(strings.ToLower(normalized), "you've reached your weekly chat limit") {
			appendLlamaErrorLog("AI credits exhausted: weekly chat limit response from model")
			appendLlamaServerLog("[CLASSIFY FAILED] AI credits exhausted (weekly chat limit reached)")
			return "", fmt.Errorf("%s\nuser has run out of ai credits", normalized)
		}

		if isToolsOnlyResponse(normalized) {
			appendLlamaServerLog(fmt.Sprintf("[CLASSIFY RETRY] tools-only response on attempt %d/%d, waiting before retry", attempt+1, 3))
			if attempt < 2 {
				if err := sleepWithContext(ctx, classifyRetryDelay(attempt, 15*time.Second)); err != nil {
					return "", err
				}
				continue
			}
			appendLlamaServerLog("[CLASSIFY FAILED] tools-only response exhausted all inner retries")
			return "", fmt.Errorf("model returned tools-only response after %d attempts", attempt+1)
		}

		if hasEmptyMessageNoise(normalized) || normalized == "" {
			appendLlamaServerLog(fmt.Sprintf("[CLASSIFY RETRY] empty-message noise on attempt %d/%d, waiting before retry", attempt+1, 3))
			if attempt < 2 {
				if err := sleepWithContext(ctx, classifyRetryDelay(attempt, classifyRetryBackoff)); err != nil {
					return "", err
				}
				continue
			}
			appendLlamaServerLog("[CLASSIFY FAILED] empty-message noise exhausted all inner retries")
			return "", fmt.Errorf("model returned empty-message noise after %d attempts", attempt+1)
		}

		searchText := stripTransientNoise(labelSearchScope(normalized))
		appendLlamaServerLog(fmt.Sprintf("[CLASSIFY RESPONSE] %s", strings.SplitN(searchText, "\n", 2)[0]))

		for _, line := range strings.Split(searchText, "\n") {
			line = strings.TrimSpace(line)
			for _, label := range allowedLabels {
				if strings.EqualFold(line, label) {
					return label, nil
				}
			}
		}

		lines := strings.Split(searchText, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return l, nil
			}
		}
		return normalized, nil
	}

	return "", fmt.Errorf("classify retry limit reached")
}

func (c *HTTPClient) classifyOnce(ctx context.Context, prompt string) (string, error) {
	payload := map[string]any{
		"model":      c.model,
		"prompt":     prompt,
		"stream":     false,
		"keep_alive": "10m",
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.path, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		body := strings.TrimSpace(string(bodyBytes))
		if body != "" {
			return "", fmt.Errorf("ollama classify failed: status %d body: %s", resp.StatusCode, body)
		}
		return "", fmt.Errorf("ollama classify failed: status %d", resp.StatusCode)
	}

	var out struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return strings.TrimSpace(string(bodyBytes)), nil
	}
	if strings.TrimSpace(out.Error) != "" {
		return "", fmt.Errorf("ollama classify failed: %s", strings.TrimSpace(out.Error))
	}
	return strings.TrimSpace(out.Response), nil
}

func (c *HTTPClient) ensureWarm(ctx context.Context) error {
	state := getWarmupState(c.baseURL + c.path + "|" + c.model)

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
	appendLlamaServerLog("[OLLAMA WARMUP] starting")
	warmCtx, cancel := context.WithTimeout(ctx, warmupRequestTimeout)
	defer cancel()

	if err := c.pullModel(warmCtx); err != nil {
		return err
	}

	_, err := c.classifyOnce(warmCtx, "Respond with exactly: READY")
	if err != nil {
		return err
	}
	appendLlamaServerLog("[OLLAMA WARMUP] model ready")
	return nil
}

func (c *HTTPClient) pullModel(ctx context.Context) error {
	payload := map[string]any{
		"model":  c.model,
		"stream": false,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/pull", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama pull failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama pull failed: status %d body: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	appendLlamaServerLog("[OLLAMA WARMUP] model pulled")
	return nil
}

func getWarmupState(key string) *warmupState {
	warmupStatesMu.Lock()
	defer warmupStatesMu.Unlock()
	if state, ok := warmupStates[key]; ok {
		return state
	}
	state := &warmupState{}
	warmupStates[key] = state
	return state
}

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

func classifyRetryDelay(attempt int, subsequent time.Duration) time.Duration {
	if attempt == 0 {
		return classifyFirstBackoff
	}
	return subsequent
}

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

func buildRuntimePrompt(tuningTemplate string, allowedLabels []string, sender, subject, body string) string {
	body = strings.TrimSpace(body)
	sender = strings.TrimSpace(sender)
	subject = strings.TrimSpace(subject)
	tuningTemplate = strings.TrimSpace(tuningTemplate)

	emailLines := make([]string, 0, 4)
	if sender != "" {
		emailLines = append(emailLines, "Email Address: "+sender)
	}
	if subject != "" {
		emailLines = append(emailLines, "Subject Line: "+subject)
	}
	if body != "" {
		emailLines = append(emailLines, body)
	}
	emailBlock := strings.TrimSpace(strings.Join(emailLines, "\n"))

	if tuningTemplate != "" {
		const placeholder = "[Insert Email Content Here]"
		if strings.Contains(tuningTemplate, placeholder) {
			return strings.Replace(tuningTemplate, placeholder, emailBlock, 1)
		}
		return strings.TrimSpace(tuningTemplate + "\n\n## 4. Input Email to Classify\n" + emailBlock)
	}

	parts := make([]string, 0, 8)
	if len(allowedLabels) > 0 {
		parts = append(parts, "Classify this email.")
		parts = append(parts, "Return exactly one label from this list and nothing else: "+strings.Join(allowedLabels, ", "))
		parts = append(parts, "No explanations, no markdown, no punctuation beyond the label text.")
		parts = append(parts, "")
	}
	if emailBlock != "" {
		parts = append(parts, emailBlock)
	}
	return strings.Join(parts, "\n")
}

// ParseAllowedLabels extracts the bullet-list items under the "## Allowed Labels" heading from a TUNING.md document.
func ParseAllowedLabels(text string) []string {
	var labels []string
	seen := map[string]bool{}
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(trimmed, "## ") && strings.Contains(lower, "allowed labels") {
			inSection = true
			continue
		}
		if inSection {
			if strings.HasPrefix(trimmed, "## ") {
				break
			}
			if strings.HasPrefix(trimmed, "- ") {
				if label := strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")); label != "" {
					key := strings.ToLower(label)
					if seen[key] {
						continue
					}
					seen[key] = true
					labels = append(labels, label)
				}
			}
		}
	}
	return labels
}

func isToolsOnlyResponse(raw string) bool {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return false
	}
	for {
		lines := strings.Split(normalized, "\n")
		if len(lines) == 0 {
			break
		}
		first := strings.TrimSpace(lines[0])
		if strings.HasPrefix(first, "[") && strings.HasSuffix(first, "]") {
			normalized = strings.TrimSpace(strings.Join(lines[1:], "\n"))
			continue
		}
		break
	}
	return strings.EqualFold(normalized, "tools")
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

func labelSearchScope(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= 40 {
		return trimmed
	}
	return strings.Join(lines[len(lines)-40:], "\n")
}

func LoadGuardrailText() string {
	paths := []string{}
	if envPath := strings.TrimSpace(os.Getenv("GARDRAIL_FILE")); envPath != "" {
		paths = append(paths, envPath)
	}
	paths = append(paths, "GARDRAIL.md", "/opt/llama-lab/GARDRAIL.md")

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
	paths = append(paths, "/llama_lab/config/TUNING.md", "TUNING.md", "/opt/llama-lab/TUNING.md")

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

func appendLlamaOutputLog(result string) {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/llama_lab/logs"
	}
	path := filepath.Join(logDir, "llama.log")
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
		_, _ = fmt.Fprintf(f, "[%s] [OLLAMA OUTPUT] %s\n", ts, line)
	}
}

func appendLlamaServerLog(message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/llama_lab/logs"
	}
	path := filepath.Join(logDir, "llama-server.log")
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

func appendLlamaErrorLog(message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	logDir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if logDir == "" {
		logDir = "/llama_lab/logs"
	}
	path := filepath.Join(logDir, "llama.err.log")
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
		_, _ = fmt.Fprintf(f, "[%s] [LLAMA ERROR] %s\n", ts, line)
	}
}

func buildWarmupPrompt(name, content string) string {
	title := strings.ToUpper(strings.TrimSpace(name))
	if title == "" {
		title = "DOCUMENT"
	}
	payload := strings.TrimSpace(content)
	if payload == "" {
		payload = "(empty)"
	}
	return fmt.Sprintf("Please acknowledge loading this %s context and reply with READY.\n\n%s", title, payload)
}

func _legacyPayloadForDebug(prompt string) []byte {
	return []byte(fmt.Sprintf("{\"prompt\":%s}", strconv.Quote(prompt)))
}
