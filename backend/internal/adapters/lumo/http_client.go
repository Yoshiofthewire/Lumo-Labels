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
	"time"
)

type HTTPClient struct {
	baseURL   string
	apiKey    string
	path      string
	guardrail string
	tuning    string
	client    *http.Client
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
	basePrompt := fmt.Sprintf("Please reply with a label from the list of [%s], for an email from [%s] with subject [%s] and the body [%s]", strings.Join(allowedLabels, ", "), sender, subject, body)
	parts := make([]string, 0, 3)
	if c.guardrail != "" {
		parts = append(parts, c.guardrail)
	}
	if c.tuning != "" {
		parts = append(parts, c.tuning)
	}
	parts = append(parts, basePrompt)
	prompt := strings.Join(parts, "\n\n")
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
		if normalized == "Tools" {
			if attempt < 2 {
				time.Sleep(15 * time.Second)
				continue
			}
		}

		// Find the first line that matches an allowed label (case-insensitive)
		for _, line := range strings.Split(normalized, "\n") {
			line = strings.TrimSpace(line)
			for _, label := range allowedLabels {
				if strings.EqualFold(line, label) {
					return label, nil
				}
			}
		}
		// No exact match — return the last non-empty line as best effort
		lines := strings.Split(normalized, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return l, nil
			}
		}
		return normalized, nil
	}

	return "", fmt.Errorf("lumo classify retry limit reached")
}

func (c *HTTPClient) classifyOnce(ctx context.Context, payload []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.path, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
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
	return cmd.Run()
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
