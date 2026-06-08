package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lumo-lab/backend/internal/adapters/lumo"
	"lumo-lab/backend/internal/adapters/proton"
	"lumo-lab/backend/internal/config"
	"lumo-lab/backend/internal/health"
	"lumo-lab/backend/internal/logging"
	"lumo-lab/backend/internal/state"
)

type Server struct {
	mu             sync.RWMutex
	cfg            config.Config
	logger         *logging.Logger
	store          *state.Store
	health         *health.Service
	configPath     string
	logPath        string
	adminPath      string
	tuningPath     string
	lumoAuthPath   string
	protonAuthPath string
	proton         proton.Client
	sessions       map[string]time.Time
}

func NewServer(cfg config.Config, logger *logging.Logger, store *state.Store, healthSvc *health.Service, protonClient proton.Client) *Server {
	configPath := filepath.Join(envOrDefault("CONFIG_DIR", "/lumo_lab/config"), "config.yaml")
	logPath := filepath.Join(envOrDefault("LOG_DIR", "/lumo_lab/logs"), "app.log")
	adminPath := filepath.Join(envOrDefault("CONFIG_DIR", "/lumo_lab/config"), "admin.env")
	tuningPath := resolveTuningPath()
	lumoAuthPath := envOrDefault("LUMO_AUTH_FILE", "/lumo_lab/config/lumo-auth.json")
	protonAuthPath := envOrDefault("PROTON_AUTH_FILE", "/lumo_lab/config/proton-auth.json")
	return &Server{cfg: cfg, logger: logger, store: store, health: healthSvc, configPath: configPath, logPath: logPath, adminPath: adminPath, tuningPath: tuningPath, lumoAuthPath: lumoAuthPath, protonAuthPath: protonAuthPath, proton: protonClient, sessions: map[string]time.Time{}}
}

func (s *Server) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/health/repair", s.withAuth(s.handleRepair))
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/me", s.handleMe)
	mux.HandleFunc("/api/auth/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("/api/auth/password", s.withAuth(s.handleChangePassword))
	mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("/api/labels", s.withAuth(s.handleLabels))
	mux.HandleFunc("/api/decisions", s.withAuth(s.handleDecisions))
	mux.HandleFunc("/api/logs", s.withAuth(s.handleLogs))
	mux.HandleFunc("/api/logs/list", s.withAuth(s.handleLogsList))
	mux.HandleFunc("/api/lumo/auth", s.withAuth(s.handleLumoAuth))
	mux.HandleFunc("/api/proton/auth", s.withAuth(s.handleProtonAuth))
	mux.HandleFunc("/api/lumo/test", s.withAuth(s.handleLumoTest))
	mux.HandleFunc("/api/tuning", s.withAuth(s.handleTuning))
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/", s.handleFrontend)

	port := envInt("WEB_PORT", 5866)
	s.logger.Info("api server starting", "port", strconv.Itoa(port))
	return http.ListenAndServe(":"+strconv.Itoa(port), mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	st := s.health.GetStatus()
	status := http.StatusOK
	if !st.Healthy {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, st)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	resp := map[string]any{
		"scanIntervalMinutes": cfg.Scan.IntervalMinutes,
		"rateLimits":          cfg.RateLimits,
		"checkpoint":          s.store.Checkpoint(),
		"serverTimeUtc":       time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		cfg := s.cfg
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPut:
		var next config.Config
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			http.Error(w, "invalid config payload", http.StatusBadRequest)
			return
		}
		if err := config.Save(s.configPath, next); err != nil {
			http.Error(w, "failed to save config", http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		s.cfg = next
		s.mu.Unlock()
		s.logger.Info("config updated via api")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	writeJSON(w, http.StatusOK, s.store.Decisions(limit))
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	configured := append([]string{}, s.cfg.Labels.Allowlist...)
	s.mu.RUnlock()

	protonLabels := []string{}
	if s.proton != nil {
		found, err := s.proton.ListLabels(r.Context())
		if err == nil {
			protonLabels = found
		}
	}
	sort.Strings(protonLabels)
	writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "proton": protonLabels})
}

func (s *Server) handleTuning(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(s.tuningPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusOK, map[string]any{"content": ""})
				return
			}
			http.Error(w, "failed to read tuning file", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"content": string(b), "path": s.tuningPath})
	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(s.tuningPath), 0o755); err != nil {
			http.Error(w, "failed to create tuning directory", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(s.tuningPath, []byte(req.Content), 0o600); err != nil {
			http.Error(w, "failed to save tuning file", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": s.tuningPath})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLumoAuth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(s.lumoAuthPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusOK, map[string]any{
					"exists":       false,
					"path":         s.lumoAuthPath,
					"localEnabled": strings.EqualFold(envOrDefault("LUMO_LOCAL_ENABLED", "true"), "true"),
				})
				return
			}
			http.Error(w, "failed to read lumo auth status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exists":       true,
			"path":         s.lumoAuthPath,
			"size":         info.Size(),
			"modifiedAt":   info.ModTime().UTC().Format(time.RFC3339),
			"localEnabled": strings.EqualFold(envOrDefault("LUMO_LOCAL_ENABLED", "true"), "true"),
		})
	case http.MethodPost:
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "invalid multipart request", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("authFile")
		if err != nil {
			http.Error(w, "authFile is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		payload, err := io.ReadAll(io.LimitReader(file, 8<<20))
		if err != nil {
			http.Error(w, "failed to read auth file", http.StatusBadRequest)
			return
		}
		if len(strings.TrimSpace(string(payload))) == 0 {
			http.Error(w, "auth file is empty", http.StatusBadRequest)
			return
		}
		var parsed map[string]any
		if err := json.Unmarshal(payload, &parsed); err != nil {
			http.Error(w, "auth file is not valid json", http.StatusBadRequest)
			return
		}
		if len(parsed) == 0 {
			http.Error(w, "auth file json is empty", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(s.lumoAuthPath), 0o755); err != nil {
			http.Error(w, "failed to create auth directory", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(s.lumoAuthPath, payload, 0o600); err != nil {
			http.Error(w, "failed to save auth file", http.StatusInternalServerError)
			return
		}
		if err := restartLumoProcess(r.Context()); err != nil {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"ok":           true,
				"path":         s.lumoAuthPath,
				"filename":     header.Filename,
				"restartOk":    false,
				"restartError": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"path":      s.lumoAuthPath,
			"filename":  header.Filename,
			"restartOk": true,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleProtonAuth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(s.protonAuthPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusOK, map[string]any{"exists": false, "path": s.protonAuthPath, "parseOk": false})
				return
			}
			http.Error(w, "failed to read proton auth status", http.StatusInternalServerError)
			return
		}
		parseOk := false
		if _, _, _, err := readProtonTokenFile(s.protonAuthPath); err == nil {
			parseOk = true
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exists":     true,
			"path":       s.protonAuthPath,
			"size":       info.Size(),
			"modifiedAt": info.ModTime().UTC().Format(time.RFC3339),
			"parseOk":    parseOk,
		})
	case http.MethodPost:
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			http.Error(w, "invalid multipart request", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("authFile")
		if err != nil {
			http.Error(w, "authFile is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		payload, err := io.ReadAll(io.LimitReader(file, 16<<20))
		if err != nil {
			http.Error(w, "failed to read auth file", http.StatusBadRequest)
			return
		}
		if len(strings.TrimSpace(string(payload))) == 0 {
			http.Error(w, "auth file is empty", http.StatusBadRequest)
			return
		}
		uid, access, refresh, clientID, err := extractProtonTokensFromStorageState(payload)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
			return
		}

		if err := os.MkdirAll(filepath.Dir(s.protonAuthPath), 0o755); err != nil {
			http.Error(w, "failed to create proton auth directory", http.StatusInternalServerError)
			return
		}
		content, err := json.MarshalIndent(map[string]any{
			"uid":          uid,
			"accessToken":  access,
			"refreshToken": refresh,
			"source":       "lumo-storage-state",
			"clientID":     clientID,
			"updatedAt":    time.Now().UTC().Format(time.RFC3339),
		}, "", "  ")
		if err != nil {
			http.Error(w, "failed to encode proton auth output", http.StatusInternalServerError)
			return
		}
		tmpPath := s.protonAuthPath + ".tmp"
		if err := os.WriteFile(tmpPath, content, 0o600); err != nil {
			http.Error(w, "failed to write proton auth file", http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmpPath, s.protonAuthPath); err != nil {
			http.Error(w, "failed to finalize proton auth file", http.StatusInternalServerError)
			return
		}

		lumoAuthUpdated := false
		lumoAuthError := ""
		if strings.TrimSpace(s.lumoAuthPath) != "" {
			if err := os.MkdirAll(filepath.Dir(s.lumoAuthPath), 0o755); err != nil {
				lumoAuthError = err.Error()
			} else if err := os.WriteFile(s.lumoAuthPath, payload, 0o600); err != nil {
				lumoAuthError = err.Error()
			} else {
				lumoAuthUpdated = true
			}
		}

		scheduleContainerRestart(s.logger, "proton auth updated", 750*time.Millisecond)

		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":               true,
			"path":             s.protonAuthPath,
			"filename":         header.Filename,
			"conversionMethod": "cookie-extract",
			"lumoAuthPath":     s.lumoAuthPath,
			"lumoAuthUpdated":  lumoAuthUpdated,
			"lumoAuthError":    lumoAuthError,
			"restartRequested": true,
			"nextAction":       "Automatic restart requested to apply new Proton auth file.",
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lines := 200
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 5000 {
			lines = v
		}
	}
	logDir := envOrDefault("LOG_DIR", "/lumo_lab/logs")
	// Resolve requested file — default to app.log, allow any *.log in logDir
	filename := filepath.Base(r.URL.Query().Get("file"))
	if filename == "" || filename == "." {
		filename = "app.log"
	}
	// Security: only allow .log files, no path traversal
	if filepath.Ext(filename) != ".log" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.Error(w, "invalid log file", http.StatusBadRequest)
		return
	}
	target := filepath.Join(logDir, filename)
	out, err := tailLines(target, lines)
	if err != nil {
		http.Error(w, "failed to read logs", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": out, "file": filename})
}

func (s *Server) handleLogsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logDir := envOrDefault("LOG_DIR", "/lumo_lab/logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		http.Error(w, "failed to list logs", http.StatusInternalServerError)
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
			files = append(files, e.Name())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b, err := os.ReadFile(s.adminPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		http.Error(w, "failed to read setup state", http.StatusInternalServerError)
		return
	}
	resp := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		resp[strings.ToLower(parts[0])] = parts[1]
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "setup": map[string]any{"admin_user": resp["admin_user"], "must_change_password": strings.EqualFold(resp["must_change_password"], "true")}})
}

func (s *Server) handleRepair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.logger.Error("manual repair requested")
	scheduleContainerRestart(s.logger, "manual repair", 250*time.Millisecond)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": "restart requested"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	admin, err := readAdminEnv(s.adminPath)
	if err != nil {
		http.Error(w, "auth config unavailable", http.StatusInternalServerError)
		return
	}
	if req.Username != admin["ADMIN_USER"] || req.Password != admin["ADMIN_PASS"] {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	token, err := randomToken(24)
	if err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "lumo_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true")})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie("lumo_session")
	if err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "lumo_session", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorize(r) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	admin, err := readAdminEnv(s.adminPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":      true,
		"username":           admin["ADMIN_USER"],
		"mustChangePassword": strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true"),
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username    string `json:"username"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.NewPassword) == "" {
		http.Error(w, "new password required", http.StatusBadRequest)
		return
	}
	admin, err := readAdminEnv(s.adminPath)
	if err != nil {
		http.Error(w, "auth config unavailable", http.StatusInternalServerError)
		return
	}
	mustChange := strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true")
	if !mustChange && req.OldPassword != admin["ADMIN_PASS"] {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if mustChange && strings.TrimSpace(req.OldPassword) != "" && req.OldPassword != admin["ADMIN_PASS"] {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	admin["ADMIN_PASS"] = req.NewPassword
	admin["MUST_CHANGE_PASSWORD"] = "false"
	if err := writeAdminEnv(s.adminPath, admin); err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLumoTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	baseURL := strings.TrimSpace(cfg.Lumo.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LUMO_BASE_URL"))
	}
	if baseURL == "" {
		http.Error(w, "lumo base url is not configured", http.StatusBadRequest)
		return
	}

	path := strings.TrimSpace(cfg.Lumo.ClassifyPath)
	if path == "" {
		path = "/"
	}
	apiKey := strings.TrimSpace(cfg.Lumo.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("LUMO_API_KEY"))
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "Email Address: test@example.com\nSubject Line: Lumo connectivity test\nReturn only the label Questionable"
	}

	allowed := cfg.Labels.Allowlist
	if len(allowed) == 0 {
		allowed = []string{"Questionable", "Important"}
	}

	guardrail := lumo.LoadGuardrailText()
	tuning := lumo.LoadTuningText()
	client := lumo.NewHTTPClient(baseURL, apiKey, path, guardrail, tuning, 120*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	result, err := client.Classify(ctx, allowed, "test@example.com", "Lumo connectivity test", prompt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"response": result,
		"baseUrl":  baseURL,
		"path":     path,
	})
}

func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	frontendDir := envOrDefault("FRONTEND_DIR", "/opt/lumo-lab/frontend")
	indexPath := filepath.Join(frontendDir, "index.html")

	requestPath := path.Clean("/" + r.URL.Path)
	relPath := strings.TrimPrefix(requestPath, "/")

	if relPath != "" {
		assetPath := filepath.Join(frontendDir, relPath)
		rootPrefix := filepath.Clean(frontendDir) + string(os.PathSeparator)
		if strings.HasPrefix(filepath.Clean(assetPath)+string(os.PathSeparator), rootPrefix) {
			if info, err := os.Stat(assetPath); err == nil && !info.IsDir() {
				http.ServeFile(w, r, assetPath)
				return
			}
		}
	}

	if _, err := os.Stat(indexPath); err == nil {
		http.ServeFile(w, r, indexPath)
		return
	}

	http.Error(w, "frontend assets not found; build frontend and set FRONTEND_DIR", http.StatusNotFound)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorize(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) authorize(r *http.Request) bool {
	cookie, err := r.Cookie("lumo_session")
	if err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := s.sessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expiresAt) {
		delete(s.sessions, cookie.Value)
		return false
	}

	// Sliding window session expiry for active users.
	s.sessions[cookie.Value] = time.Now().Add(24 * time.Hour)
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func scheduleContainerRestart(logger *logging.Logger, reason string, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		if logger != nil {
			logger.Error("container restart requested", "reason", reason)
		}
		_ = syscall.Kill(1, syscall.SIGTERM)
		os.Exit(2)
	}()
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envOrDefault(name, fallback string) string {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	return v
}

func resolveTuningPath() string {
	if envPath := strings.TrimSpace(os.Getenv("TUNING_FILE")); envPath != "" {
		return envPath
	}
	candidates := []string{"/lumo_lab/config/TUNING.md", "TUNING.md", "/opt/lumo-lab/TUNING.md"}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/lumo_lab/config/TUNING.md"
}

func restartLumoProcess(ctx context.Context) error {
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "supervisorctl", args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	out, err := run("-c", "/etc/supervisord.conf", "restart", "lumo")
	if err == nil {
		return nil
	}

	msg := out
	if msg == "" {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "not running") || strings.Contains(lower, "spawn error") || strings.Contains(lower, "fatal") {
		startOut, startErr := run("-c", "/etc/supervisord.conf", "start", "lumo")
		if startErr == nil {
			return nil
		}
		if strings.TrimSpace(startOut) != "" {
			msg = msg + "; start attempt: " + strings.TrimSpace(startOut)
		}
	}

	return fmt.Errorf("restart lumo: %s", msg)
}

type storageStateCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

type storageState struct {
	Cookies []storageStateCookie `json:"cookies"`
}

type refreshCookiePayload struct {
	ClientID     string `json:"ClientID"`
	RefreshToken string `json:"RefreshToken"`
	UID          string `json:"UID"`
}

func extractProtonTokensFromStorageState(payload []byte) (string, string, string, string, error) {
	var state storageState
	if err := json.Unmarshal(payload, &state); err != nil {
		return "", "", "", "", errors.New("storageState auth file is not valid json")
	}
	if len(state.Cookies) == 0 {
		return "", "", "", "", errors.New("storageState auth file has no cookies")
	}

	type refreshData struct {
		refreshToken string
		clientID     string
		domain       string
	}
	accessByUID := map[string]string{}
	refreshByUID := map[string]refreshData{}

	for _, cookie := range state.Cookies {
		if strings.HasPrefix(cookie.Name, "AUTH-") {
			uid := strings.TrimPrefix(cookie.Name, "AUTH-")
			if strings.TrimSpace(uid) != "" && strings.TrimSpace(cookie.Value) != "" {
				if _, ok := accessByUID[uid]; !ok || strings.Contains(cookie.Domain, "account.proton.me") {
					accessByUID[uid] = strings.TrimSpace(cookie.Value)
				}
			}
		}
		if strings.HasPrefix(cookie.Name, "REFRESH-") {
			decoded, err := url.QueryUnescape(cookie.Value)
			if err != nil {
				continue
			}
			var parsed refreshCookiePayload
			if err := json.Unmarshal([]byte(decoded), &parsed); err != nil {
				continue
			}
			uid := strings.TrimSpace(parsed.UID)
			refresh := strings.TrimSpace(parsed.RefreshToken)
			if uid == "" || refresh == "" {
				continue
			}
			current, ok := refreshByUID[uid]
			if !ok || strings.EqualFold(parsed.ClientID, "WebAccount") {
				refreshByUID[uid] = refreshData{refreshToken: refresh, clientID: strings.TrimSpace(parsed.ClientID), domain: cookie.Domain}
				continue
			}
			if strings.Contains(cookie.Domain, "account.proton.me") && !strings.Contains(current.domain, "account.proton.me") {
				refreshByUID[uid] = refreshData{refreshToken: refresh, clientID: strings.TrimSpace(parsed.ClientID), domain: cookie.Domain}
			}
		}
	}

	selectedUID := ""
	selectedClientID := ""
	for uid, refresh := range refreshByUID {
		if _, ok := accessByUID[uid]; !ok {
			continue
		}
		if selectedUID == "" || strings.EqualFold(refresh.clientID, "WebAccount") {
			selectedUID = uid
			selectedClientID = refresh.clientID
			if strings.EqualFold(refresh.clientID, "WebAccount") {
				break
			}
		}
	}
	if selectedUID == "" {
		return "", "", "", "", errors.New("could not extract matching AUTH/REFRESH token pair from storageState cookies")
	}
	refresh := refreshByUID[selectedUID].refreshToken
	access := accessByUID[selectedUID]
	if strings.TrimSpace(refresh) == "" || strings.TrimSpace(access) == "" {
		return "", "", "", "", errors.New("extracted proton token pair is incomplete")
	}
	return selectedUID, access, refresh, selectedClientID, nil
}

func readProtonTokenFile(path string) (string, string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	var parsed struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(parsed.UID) == "" || strings.TrimSpace(parsed.AccessToken) == "" || strings.TrimSpace(parsed.RefreshToken) == "" {
		return "", "", "", errors.New("incomplete proton auth file")
	}
	return strings.TrimSpace(parsed.UID), strings.TrimSpace(parsed.AccessToken), strings.TrimSpace(parsed.RefreshToken), nil
}

func tailLines(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]string, 0, limit)
	s := bufio.NewScanner(f)
	for s.Scan() {
		buf = append(buf, s.Text())
		if len(buf) > limit {
			buf = buf[1:]
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return buf, nil
}

func readAdminEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

func writeAdminEnv(path string, kv map[string]string) error {
	content := fmt.Sprintf("ADMIN_USER=%s\nADMIN_PASS=%s\nMUST_CHANGE_PASSWORD=%s\n", kv["ADMIN_USER"], kv["ADMIN_PASS"], kv["MUST_CHANGE_PASSWORD"])
	return os.WriteFile(path, []byte(content), 0o600)
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
