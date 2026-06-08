package processor

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"lumo-lab/backend/internal/adapters/lumo"
	"lumo-lab/backend/internal/adapters/proton"
	"lumo-lab/backend/internal/config"
	"lumo-lab/backend/internal/health"
	"lumo-lab/backend/internal/logging"
	"lumo-lab/backend/internal/redaction"
	"lumo-lab/backend/internal/state"
)

type Poller struct {
	cfg       config.Config
	log       *logging.Logger
	store     *state.Store
	health    *health.Service
	proton    proton.Client
	lumo      lumo.Client
	redaction *redaction.Engine
	cancel    context.CancelFunc

	mu        sync.Mutex
	processed []time.Time
}

func New(cfg config.Config, log *logging.Logger, store *state.Store, healthSvc *health.Service, protonClient proton.Client, lumoClient lumo.Client) (*Poller, error) {
	re, err := redaction.New(cfg.Redaction.Patterns)
	if err != nil {
		return nil, err
	}
	return &Poller{cfg: cfg, log: log, store: store, health: healthSvc, proton: protonClient, lumo: lumoClient, redaction: re, processed: []time.Time{}}, nil
}

func (p *Poller) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	interval := time.Duration(p.cfg.Scan.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	p.log.Info("poller started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.tick()
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
			p.health.SetStatus(health.Status{Healthy: true, FailureReason: []string{"Proton Mail Auth Unhealthy"}})
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
	if strings.Contains(msg, "422") {
		return true
	}
	if strings.Contains(msg, "out of date") || strings.Contains(msg, "refresh the page") {
		return true
	}
	return false
}

func (p *Poller) handleMessage(ctx context.Context, msg proton.Message) error {
	body := strings.TrimSpace(msg.Body)
	if len(body) > 2000 {
		body = body[:2000]
	}
	redacted := p.redaction.Apply(body)
	label, err := classifyWithRetry(ctx, p.lumo, p.cfg.Labels.Allowlist, msg.Sender, msg.Subject, redacted)
	if err != nil {
		return err
	}
	selected := lumo.SelectLabelFromText(p.cfg.Labels.Allowlist, label)
	if selected == "" {
		_ = p.store.AddDecision(state.Decision{
			MessageID: msg.ID,
			Sender:    msg.Sender,
			Subject:   msg.Subject,
			Status:    "skipped",
			Detail:    "no known label returned",
		})
		return p.store.MarkProcessed(msg.ID)
	}
	if err := applyLabelWithRetry(ctx, p.proton, msg.ID, selected); err != nil {
		return err
	}
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

func classifyWithRetry(ctx context.Context, c lumo.Client, labels []string, sender, subject, body string) (string, error) {
	var out string
	var err error
	for i := 0; i < 3; i++ {
		out, err = c.Classify(ctx, labels, sender, subject, body)
		if err == nil {
			return out, nil
		}
		if i < 2 {
			time.Sleep(5 * time.Second)
		}
	}
	return "", err
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
