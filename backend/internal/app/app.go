package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lumo-lab/backend/internal/adapters/lumo"
	"lumo-lab/backend/internal/adapters/proton"
	"lumo-lab/backend/internal/api"
	"lumo-lab/backend/internal/config"
	"lumo-lab/backend/internal/health"
	"lumo-lab/backend/internal/logging"
	"lumo-lab/backend/internal/processor"
	"lumo-lab/backend/internal/state"
)

// Run dispatches the process mode and blocks until shutdown for long-running modes.
func Run(args []string) error {
	fs := flag.NewFlagSet("lumo-lab", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process mode: daemon, server, all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths := config.Paths{
		ConfigFile: filepath.Join(envOrDefault("CONFIG_DIR", "/lumo_lab/config"), "config.yaml"),
		StateDir:   envOrDefault("STATE_DIR", "/lumo_lab/state"),
		LogDir:     envOrDefault("LOG_DIR", "/lumo_lab/logs"),
	}

	cfg, err := config.LoadOrInit(paths.ConfigFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Timezone == "" {
		cfg.Timezone = "America/New_York"
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", cfg.Timezone, err)
	}

	// Auto-populate label allowlist from TUNING.md when the config has none.
	if len(cfg.Labels.Allowlist) == 0 {
		if labels := lumo.ParseAllowedLabels(lumo.LoadTuningText()); len(labels) > 0 {
			cfg.Labels.Allowlist = labels
		}
	}

	logger, err := logging.New(paths.LogDir, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Close()

	store, err := state.New(paths.StateDir)
	if err != nil {
		return fmt.Errorf("create state store: %w", err)
	}

	healthSvc := health.NewService()
	healthSvc.MarkHealthy()

	switch *mode {
	case "daemon":
		return runDaemon(cfg, logger, store, healthSvc)
	case "server":
		return runServer(cfg, logger, store, healthSvc)
	case "all":
		return runAll(cfg, logger, store, healthSvc)
	default:
		return errors.New("invalid mode; expected daemon, server, or all")
	}
}

func runDaemon(cfg config.Config, logger *logging.Logger, store *state.Store, healthSvc *health.Service) error {
	lumoClient := newLumoClient(cfg)
	poller, err := processor.New(cfg, logger, store, healthSvc, newProtonClient(), lumoClient)
	if err != nil {
		return err
	}
	warmupLumoOnStartup(logger, lumoClient, poller)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go poller.Run()
	logger.Info("poller goroutine started")
	go monitorHealth(logger, healthSvc)
	<-stop
	poller.Stop()
	return nil
}

func runServer(cfg config.Config, logger *logging.Logger, store *state.Store, healthSvc *health.Service) error {
	srv := api.NewServer(cfg, logger, store, healthSvc, newProtonClient())
	return srv.Run()
}

func runAll(cfg config.Config, logger *logging.Logger, store *state.Store, healthSvc *health.Service) error {
	// Restore the sticky AI-credits flag onto the health status so a restart
	// keeps surfacing it until a successful classify clears it.
	if exhausted, at := store.AICreditsExhausted(); exhausted {
		healthSvc.SetAICreditsExhausted(at)
	}
	protonClient := newProtonClient()
	srv := api.NewServer(cfg, logger, store, healthSvc, protonClient)
	lumoClient := newLumoClient(cfg)
	poller, err := processor.New(cfg, logger, store, healthSvc, protonClient, lumoClient)
	if err != nil {
		return err
	}
	warmupLumoOnStartup(logger, lumoClient, poller)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go poller.Run()
	logger.Info("poller goroutine started")
	go monitorHealth(logger, healthSvc)
	go func() {
		if err := srv.Run(); err != nil {
			logger.Error("api server stopped", "error", err.Error())
		}
	}()
	<-stop
	poller.Stop()
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func monitorHealth(logger *logging.Logger, healthSvc *health.Service) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	threshold := envDurationSeconds("UNHEALTHY_RESTART_SECONDS", 300)
	for range ticker.C {
		st := healthSvc.GetStatus()
		if st.Healthy {
			continue
		}
		if st.UnhealthyFor < int64(threshold) {
			continue
		}
		logger.Error("unhealthy threshold exceeded, requesting container restart", "unhealthy_for_seconds", strconv.FormatInt(st.UnhealthyFor, 10))
		_ = syscall.Kill(1, syscall.SIGTERM)
		os.Exit(2)
	}
}

func envDurationSeconds(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func newLumoClient(cfg config.Config) lumo.Client {
	baseURL := strings.TrimSpace(os.Getenv("LUMO_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(cfg.Lumo.BaseURL)
	}
	if baseURL == "" {
		return &lumo.StubClient{}
	}
	apiKey := strings.TrimSpace(os.Getenv("LUMO_API_KEY"))
	if apiKey == "" {
		apiKey = cfg.Lumo.APIKey
	}
	classifyPath := strings.TrimSpace(cfg.Lumo.ClassifyPath)
	if classifyPath == "" {
		classifyPath = "/"
	}
	guardrail := lumo.LoadGuardrailText()
	tuning := lumo.LoadTuningText()
	return lumo.NewHTTPClient(baseURL, apiKey, classifyPath, guardrail, tuning, 3*time.Minute)
}

func newProtonClient() proton.Client {
	path := strings.TrimSpace(os.Getenv("PROTON_AUTH_FILE"))
	if path == "" {
		path = "/lumo_lab/config/proton-auth.json"
	}
	if _, err := os.Stat(path); err == nil {
		return proton.NewAPIClientFromEnv()
	}
	return &proton.StubClient{}
}

func warmupLumoOnStartup(logger *logging.Logger, client lumo.Client, trigger interface{ TriggerNow() }) {
	type warmupClient interface {
		Warmup(ctx context.Context) error
	}

	w, ok := client.(warmupClient)
	if !ok {
		// No warmup needed; trigger the first sweep immediately.
		if trigger != nil {
			go trigger.TriggerNow()
		}
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		logger.Info("lumo startup warmup requested")
		if err := w.Warmup(ctx); err != nil {
			logger.Error("lumo startup warmup failed", "error", err.Error())
			return
		}
		logger.Info("lumo startup warmup completed")
		if trigger != nil {
			logger.Info("processing unread unlabeled mail after startup warmup")
			type unreadSweepTrigger interface {
				TriggerUnreadSweep()
			}
			if sweep, ok := trigger.(unreadSweepTrigger); ok {
				sweep.TriggerUnreadSweep()
				return
			}
			trigger.TriggerNow()
		}
	}()
}
