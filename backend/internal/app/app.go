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

	"llama-lab/backend/internal/adapters/llama"
	"llama-lab/backend/internal/adapters/proton"
	"llama-lab/backend/internal/api"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/processor"
	"llama-lab/backend/internal/state"
)

// Run dispatches the process mode and blocks until shutdown for long-running modes.
func Run(args []string) error {
	fs := flag.NewFlagSet("llama-lab", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process mode: daemon, server, all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths := config.Paths{
		ConfigFile: filepath.Join(envOrDefault("CONFIG_DIR", "/llama_lab/config"), "config.yaml"),
		StateDir:   envOrDefault("STATE_DIR", "/llama_lab/state"),
		LogDir:     envOrDefault("LOG_DIR", "/llama_lab/logs"),
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
		if labels := llama.ParseAllowedLabels(llama.LoadTuningText()); len(labels) > 0 {
			cfg.Labels.Allowlist = labels
		}
	}

	logger, err := logging.New(paths.LogDir, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Close()
	cleanupStaleProtonAuthTempFiles(logger)
	logProtonAuthStartupState(logger)

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
	llamaClient := newLlamaClient(cfg)
	poller, err := processor.New(cfg, logger, store, healthSvc, newProtonClient(), llamaClient)
	if err != nil {
		return err
	}
	warmupLlamaOnStartup(logger, llamaClient, poller)
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
	llamaClient := newLlamaClient(cfg)
	poller, err := processor.New(cfg, logger, store, healthSvc, protonClient, llamaClient)
	if err != nil {
		return err
	}
	warmupLlamaOnStartup(logger, llamaClient, poller)

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

func protonAuthFilePath() string {
	if path := strings.TrimSpace(os.Getenv("PROTON_AUTH_FILE")); path != "" {
		return path
	}
	return "/llama_lab/config/proton-auth.json"
}

func cleanupStaleProtonAuthTempFiles(logger *logging.Logger) {
	authPath := protonAuthFilePath()
	pattern := authPath + ".tmp.*"
	files, err := filepath.Glob(pattern)
	if err != nil {
		logger.Error("failed to list proton auth temp files", "pattern", pattern, "error", err.Error())
		return
	}
	if len(files) == 0 {
		return
	}

	cutoff := time.Now().Add(-5 * time.Minute)
	removed := 0
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	if removed > 0 {
		logger.Info("removed stale proton auth temp files", "count", strconv.Itoa(removed))
	}
}

func logProtonAuthStartupState(logger *logging.Logger) {
	authPath := protonAuthFilePath()
	snapshotPath := authPath + ".last-good"
	mainExists, mainSize, mainMTime := fileStatSummary(authPath)
	snapshotExists, snapshotSize, snapshotMTime := fileStatSummary(snapshotPath)

	source := "none"
	if mainExists {
		source = "main"
	} else if snapshotExists {
		source = "snapshot"
	}

	logger.Info(
		"proton auth startup state",
		"source_candidate", source,
		"main_exists", strconv.FormatBool(mainExists),
		"main_size", strconv.FormatInt(mainSize, 10),
		"main_mtime", mainMTime,
		"snapshot_exists", strconv.FormatBool(snapshotExists),
		"snapshot_size", strconv.FormatInt(snapshotSize, 10),
		"snapshot_mtime", snapshotMTime,
	)
}

func fileStatSummary(path string) (bool, int64, string) {
	info, err := os.Stat(path)
	if err != nil {
		return false, 0, ""
	}
	return true, info.Size(), info.ModTime().UTC().Format(time.RFC3339)
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

func newLlamaClient(cfg config.Config) llama.Client {
	baseURL := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LLAMA_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	if baseURL == "" {
		return &llama.StubClient{}
	}
	apiKey := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	classifyPath := strings.TrimSpace(os.Getenv("OLLAMA_GENERATE_PATH"))
	if classifyPath == "" {
		classifyPath = "/api/generate"
	}
	tuning := llama.LoadTuningText()
	return llama.NewHTTPClient(baseURL, apiKey, classifyPath, tuning, 3*time.Minute)
}

func newProtonClient() proton.Client {
	path := strings.TrimSpace(os.Getenv("PROTON_AUTH_FILE"))
	if path == "" {
		path = "/llama_lab/config/proton-auth.json"
	}
	if _, err := os.Stat(path); err == nil {
		return proton.NewAPIClientFromEnv()
	}
	return &proton.StubClient{}
}

func warmupLlamaOnStartup(logger *logging.Logger, client llama.Client, trigger interface{ TriggerNow() }) {
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
		logger.Info("llama startup warmup requested")
		if err := w.Warmup(ctx); err != nil {
			logger.Error("llama startup warmup failed", "error", err.Error())
			return
		}
		logger.Info("llama startup warmup completed")
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
