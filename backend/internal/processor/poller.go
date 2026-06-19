package processor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"llama-lab/backend/internal/adapters/llama"
	"llama-lab/backend/internal/adapters/proton"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/redaction"
	"llama-lab/backend/internal/state"
)

type Poller struct {
	cfg       config.Config
	log       *logging.Logger
	store     *state.Store
	health    *health.Service
	proton    proton.Client
	llama     llama.Client
	redaction *redaction.Engine
	cancel    context.CancelFunc
	mu        sync.Mutex
	tickSem   chan struct{}
	processed []time.Time
}

func New(cfg config.Config, log *logging.Logger, store *state.Store, healthSvc *health.Service, protonClient proton.Client, llamaClient llama.Client) (*Poller, error) {
	re, err := redaction.New(cfg.Redaction.Patterns)
	if err != nil {
		return nil, err
	}
	p := &Poller{cfg: cfg, log: log, store: store, health: healthSvc, proton: protonClient, llama: llamaClient, redaction: re, processed: []time.Time{}}
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	return p, nil
}

func (p *Poller) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	interval := time.Duration(p.cfg.Scan.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 90 * time.Second
	}

	p.log.Info("poller started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopped")
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *Poller) TriggerNow() {
	p.tick()
}

func (p *Poller) TriggerUnreadSweep() {
	if err := p.store.SetCheckpoint(""); err != nil {
		p.log.Error("failed to reset checkpoint for unread sweep", "error", err.Error())
	}
	p.tick()
}

func (p *Poller) tick() {
	// acquire semaphore; if another tick is running, log that we're waiting
	select {
	case <-p.tickSem:
		// acquired immediately
	default:
		p.log.Info("poll tick waiting for previous tick to finish")
		<-p.tickSem
	}
	defer func() { p.tickSem <- struct{}{} }()

	if err := p.store.Cleanup(30); err != nil {
		p.log.Error("state cleanup failed", "error", err.Error())
		p.health.MarkUnhealthy("state cleanup failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	checkpoint := p.store.Checkpoint()
	messages, nextCheckpoint, err := p.proton.ListUnreadInbox(ctx, checkpoint)
	if err != nil {
		p.log.Error("fetch unread inbox failed", "error", err.Error())
		if isProtonAuthUnhealthyError(err) {
			p.log.Error("proton auth unhealthy", "checkpoint", checkpoint, "hint", protonAuthTroubleshootingHint(err))
			p.health.SetStatus(health.Status{Healthy: true, FailureReason: []string{"Proton Mail Auth Unhealthy"}})
			return
		}
		if isProtonAuthRecoverableError(err) {
			p.log.Info("proton auth refresh retrying", "checkpoint", checkpoint, "hint", protonAuthTroubleshootingHint(err))
			p.health.SetStatus(health.Status{Healthy: true, FailureReason: []string{"Proton Mail Auth Refresh Retrying"}})
			return
		}
		p.health.MarkUnhealthy("proton unreachable")
		return
	}

	processedCount := 0
	skippedSeenCount := 0
	failedCount := 0
	rateLimitedCount := 0
	for _, msg := range messages {
		if p.store.Seen(msg.ID) {
			skippedSeenCount++
			continue
		}
		if !p.allowByRate() {
			p.log.Info("rate limit reached, deferring remaining emails")
			rateLimitedCount = len(messages) - processedCount - skippedSeenCount - failedCount
			break
		}
		messageCtx, messageCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		err := p.handleMessage(messageCtx, msg)
		messageCancel()
		if err != nil {
			failedCount++
			p.log.Error("message processing failed", "message_id", msg.ID, "error", err.Error())
			_ = p.store.AddDecision(state.Decision{
				MessageID: msg.ID,
				Sender:    msg.Sender,
				Subject:   msg.Subject,
				Status:    "failed",
				Detail:    err.Error(),
			})
			// Retire the message so it is not retried on the next tick.
			_ = p.store.MarkProcessed(msg.ID)
			continue
		}
		processedCount++
	}

	if nextCheckpoint != "" {
		if err := p.store.SetCheckpoint(nextCheckpoint); err != nil {
			p.log.Error("failed to persist checkpoint", "error", err.Error())
		}
	}

	p.log.Info(
		"poll tick summary",
		"fetched", intToString(len(messages)),
		"processed", intToString(processedCount),
		"skipped_seen", intToString(skippedSeenCount),
		"failed", intToString(failedCount),
		"deferred_rate_limited", intToString(rateLimitedCount),
	)
	p.log.Info("poll tick completed")
	p.health.MarkHealthy()
}

func isProtonAuthUnhealthyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "401") {
		return true
	}
	if strings.Contains(msg, "invalid refresh token") {
		return true
	}
	if strings.Contains(msg, "de-auth") && strings.Contains(msg, "400") {
		return true
	}
	if strings.Contains(msg, "re-authentication required") {
		return true
	}
	if strings.Contains(msg, "missing proton token credentials") {
		return true
	}
	if strings.Contains(msg, "failed to parse proton auth file") {
		return true
	}
	if strings.Contains(msg, "failed to read proton auth file") {
		return true
	}
	return false
}

func isProtonAuthRecoverableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "422") {
		return true
	}
	if strings.Contains(msg, "out of date") || strings.Contains(msg, "refresh the page") {
		return true
	}
	if strings.Contains(msg, "invalid input") {
		return true
	}
	if strings.Contains(msg, "de-auth") && strings.Contains(msg, "422") {
		return true
	}
	return false
}

func protonAuthTroubleshootingHint(err error) string {
	if err == nil {
		return "Re-upload a fresh Proton mail auth file from the Config page."
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "invalid refresh token") || (strings.Contains(msg, "de-auth") && strings.Contains(msg, "400")) {
		return "Refresh token is no longer valid; export a fresh mail_auth.json and upload it."
	}
	if strings.Contains(msg, "422") || strings.Contains(msg, "invalid input") || strings.Contains(msg, "out of date") {
		return "Session/App-Version mismatch; re-export auth from Proton Mail web and upload again."
	}
	if strings.Contains(msg, "401") {
		return "Access token unauthorized; upload a fresh auth file and verify system time/TZ are correct."
	}
	return "Re-upload a fresh Proton mail auth file and verify /llama_lab/config/proton-auth.json is parseable."
}

// recentDecisionsContext returns a short summary of the last N applied decisions to give Llama labelling context.
func (p *Poller) recentDecisionsContext(limit int) string {
	all := p.store.Decisions(50)
	var applied []state.Decision
	for _, d := range all {
		if d.Status == "applied" && d.Label != "" {
			applied = append(applied, d)
			if len(applied) >= limit {
				break
			}
		}
	}
	if len(applied) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Recent labeling decisions for reference:\n")
	for _, d := range applied {
		sb.WriteString("- From: ")
		sb.WriteString(d.Sender)
		if d.Subject != "" {
			sb.WriteString(", Subject: ")
			sb.WriteString(d.Subject)
		}
		sb.WriteString(" → Label: ")
		sb.WriteString(d.Label)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (p *Poller) handleMessage(ctx context.Context, msg proton.Message) error {
	body := strings.TrimSpace(msg.Body)
	if len(body) > 2000 {
		body = body[:2000]
	}
	redacted := p.redaction.Apply(body)

	decisionsCtx := p.recentDecisionsContext(10)
	bodyWithContext := redacted
	if decisionsCtx != "" {
		if bodyWithContext != "" {
			bodyWithContext = redacted + "\n---\n" + decisionsCtx
		} else {
			bodyWithContext = decisionsCtx
		}
	}

	label, err := classifyWithRetry(ctx, p.llama, p.cfg.Labels.Allowlist, msg.Sender, msg.Subject, bodyWithContext)
	if err != nil {
		if isAICreditsExhaustedError(err) {
			p.flagAICreditsExhausted()
		}
		return err
	}
	// A successful classification means Llama has credits again; clear any flag.
	p.clearAICreditsExhausted()
	p.log.Info("classification result", "message_id", msg.ID, "raw_label", strings.TrimSpace(label), "sender", msg.Sender, "subject", msg.Subject)
	selected := llama.SelectLabelFromText(p.cfg.Labels.Allowlist, label)
	if selected == "" {
		p.log.Info("classification skipped", "message_id", msg.ID, "reason", "no known label returned", "raw_label", strings.TrimSpace(label), "allowlist_count", intToString(len(p.cfg.Labels.Allowlist)))
		_ = p.store.AddDecision(state.Decision{
			MessageID: msg.ID,
			Sender:    msg.Sender,
			Subject:   msg.Subject,
			Status:    "skipped",
			Detail:    "no known label returned",
		})
		return p.store.MarkProcessed(msg.ID)
	}
	p.log.Info("applying label", "message_id", msg.ID, "selected_label", selected, "sender", msg.Sender, "subject", msg.Subject)
	if err := applyLabelWithRetry(ctx, p.proton, msg.ID, selected); err != nil {
		p.log.Error("label apply failed", "message_id", msg.ID, "selected_label", selected, "error", err.Error())
		return err
	}
	p.log.Info("label applied", "message_id", msg.ID, "selected_label", selected)
	if err := p.store.MarkProcessed(msg.ID); err != nil {
		return err
	}
	return p.store.AddDecision(state.Decision{
		MessageID: msg.ID,
		Sender:    msg.Sender,
		Subject:   msg.Subject,
		Label:     selected,
		Status:    "applied",
		Detail:    "label applied successfully",
	})
}

func classifyWithRetry(ctx context.Context, c llama.Client, labels []string, sender, subject, body string) (string, error) {
	var out string
	var err error
	for i := 0; i < 3; i++ {
		out, err = c.Classify(ctx, labels, sender, subject, body)
		if err == nil && out != "" {
			return out, nil
		}
		if err != nil && isPermanentLlamaClassifyError(err) {
			return "", err
		}
		if err == nil {
			// Classify returned no error but an empty label — treat as retryable.
			err = fmt.Errorf("llama returned empty label")
		}
		if i < 2 {
			time.Sleep(5 * time.Second)
		}
	}
	return "", err
}

func isPermanentLlamaClassifyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "422") {
		return true
	}
	if strings.Contains(msg, "invalid input") || strings.Contains(msg, "unprocessable") {
		return true
	}
	// Out of AI credits will not recover on retry; stop hammering Llama.
	if isAICreditsExhaustedError(err) {
		return true
	}
	return false
}

// isAICreditsExhaustedError reports whether a classify error is Llama signalling
// that the weekly chat limit / AI credits have been exhausted.
func isAICreditsExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "out of ai credits") ||
		strings.Contains(msg, "weekly chat limit")
}

// flagAICreditsExhausted persists the AI-credits flag, mirrors it onto the
// health status, and logs once on the false->true transition.
func (p *Poller) flagAICreditsExhausted() {
	now := time.Now().UTC().Format(time.RFC3339)
	newly, err := p.store.SetAICreditsExhausted(now)
	if err != nil {
		p.log.Error("failed to persist ai credits exhausted flag", "error", err.Error())
	}
	p.health.SetAICreditsExhausted(now)
	if newly {
		p.log.Error("Llama AI credits exhausted; email classification paused until credits reset",
			"detail", "Llama returned the weekly chat limit response")
	}
}

// clearAICreditsExhausted resets the AI-credits flag after a successful classify.
func (p *Poller) clearAICreditsExhausted() {
	if exhausted, _ := p.store.AICreditsExhausted(); !exhausted {
		return
	}
	cleared, err := p.store.ClearAICreditsExhausted()
	if err != nil {
		p.log.Error("failed to clear ai credits exhausted flag", "error", err.Error())
	}
	p.health.ClearAICreditsExhausted()
	if cleared {
		p.log.Info("Llama AI credits restored; email classification resumed")
	}
}

func applyLabelWithRetry(ctx context.Context, c proton.Client, messageID, label string) error {
	var err error
	for i := 0; i < 3; i++ {
		err = c.EnsureLabel(ctx, label)
		if err == nil {
			err = c.ApplyLabel(ctx, messageID, label)
		}
		if err == nil {
			return nil
		}
		if i < 2 {
			time.Sleep(30 * time.Second)
		}
	}
	return err
}

func (p *Poller) allowByRate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	trimmed := make([]time.Time, 0, len(p.processed))
	for _, t := range p.processed {
		if t.After(hourCutoff) {
			trimmed = append(trimmed, t)
		}
	}
	p.processed = trimmed
	minuteCount := 0
	for _, t := range p.processed {
		if t.After(minuteCutoff) {
			minuteCount++
		}
	}
	if minuteCount >= p.cfg.RateLimits.PerMinute {
		return false
	}
	if len(p.processed) >= p.cfg.RateLimits.PerHour {
		return false
	}
	p.processed = append(p.processed, now)
	return true
}

func intToString(v int) string {
	return strconv.Itoa(v)
}
