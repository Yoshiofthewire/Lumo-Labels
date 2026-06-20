package api

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
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

	"llama-lab/backend/internal/adapters/llama"
	"llama-lab/backend/internal/adapters/proton"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/state"

	protonapi "github.com/ProtonMail/go-proton-api"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/scrypt"
)

type Server struct {
	mu                  sync.RWMutex
	cfg                 config.Config
	logger              *logging.Logger
	store               *state.Store
	health              *health.Service
	configPath          string
	logPath             string
	adminPath           string
	tuningPath          string
	llamaAuthPath       string
	protonAuthPath      string
	protonLoginPath     string
	protonLoginKeyPath  string
	protonKeyPath       string
	protonKeyEncKeyPath string
	protonPassPath      string
	proton              proton.Client
	sessions            map[string]time.Time
}

func NewServer(cfg config.Config, logger *logging.Logger, store *state.Store, healthSvc *health.Service, protonClient proton.Client) *Server {
	secretDir := envOrDefault("SECRET_DIR", "/llama_lab/private")
	configPath := filepath.Join(envOrDefault("CONFIG_DIR", "/llama_lab/config"), "config.yaml")
	logPath := filepath.Join(envOrDefault("LOG_DIR", "/llama_lab/logs"), "app.log")
	adminPath := filepath.Join(envOrDefault("CONFIG_DIR", "/llama_lab/config"), "admin.env")
	tuningPath := resolveTuningPath()
	llamaAuthPath := envOrDefault("LLAMA_AUTH_FILE", "/llama_lab/config/llama-auth.json")
	protonAuthPath := envOrDefault("PROTON_AUTH_FILE", "/llama_lab/config/proton-auth.json")
	protonLoginPath := envOrDefault("PROTON_LOGIN_FILE", "/llama_lab/private/proton-login.json")
	protonLoginKeyPath := envOrDefault("PROTON_LOGIN_KEY_FILE", "/llama_lab/private/proton-login.key")
	protonKeyPath := envOrDefault("PROTON_PRIVATE_KEY_FILE", filepath.Join(secretDir, "proton-private-key.asc"))
	protonKeyEncKeyPath := envOrDefault("PROTON_PRIVATE_KEY_ENC_KEY_FILE", filepath.Join(secretDir, "proton-private-key.key"))
	protonPassPath := envOrDefault("PROTON_PRIVATE_KEY_PASSWORD_FILE", filepath.Join(secretDir, "proton-private-key-password"))
	return &Server{cfg: cfg, logger: logger, store: store, health: healthSvc, configPath: configPath, logPath: logPath, adminPath: adminPath, tuningPath: tuningPath, llamaAuthPath: llamaAuthPath, protonAuthPath: protonAuthPath, protonLoginPath: protonLoginPath, protonLoginKeyPath: protonLoginKeyPath, protonKeyPath: protonKeyPath, protonKeyEncKeyPath: protonKeyEncKeyPath, protonPassPath: protonPassPath, proton: protonClient, sessions: map[string]time.Time{}}
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
	mux.HandleFunc("/api/llama/auth", s.withAuth(s.handleLlamaAuth))
	mux.HandleFunc("/api/proton/auth", s.withAuth(s.handleProtonAuth))
	mux.HandleFunc("/api/proton/auth/bootstrap", s.withAuth(s.handleProtonAuthBootstrap))
	mux.HandleFunc("/api/proton/login", s.withAuth(s.handleProtonLogin))
	mux.HandleFunc("/api/proton/login/validate", s.withAuth(s.handleProtonLoginValidate))
	mux.HandleFunc("/api/debug/proton-token-state", s.withAuth(s.handleProtonTokenState))
	mux.HandleFunc("/api/proton/private-key", s.withAuth(s.handleProtonPrivateKey))
	mux.HandleFunc("/api/llama/test", s.withAuth(s.handleLlamaTest))
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
		"scanIntervalSeconds": cfg.Scan.IntervalSeconds,
		"rateLimits":          cfg.RateLimits,
		"checkpoint":          s.store.Checkpoint(),
		"serverTimeUtc":       time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, resp)
}

type protonLoginPayload struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totpSecret"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
}

func (s *Server) handleProtonLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		payload, exists, err := readProtonLoginPayload(s.protonLoginPath, s.protonLoginKeyPath)
		if err != nil {
			http.Error(w, "failed to read proton login credentials", http.StatusInternalServerError)
			return
		}
		if !exists {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false, "path": s.protonLoginPath})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":      true,
			"path":            s.protonLoginPath,
			"keyPath":         s.protonLoginKeyPath,
			"username":        payload.Username,
			"hasTotpSecret":   strings.TrimSpace(payload.TOTPSecret) != "",
			"updatedAt":       payload.UpdatedAt,
			"encryptedAtRest": true,
		})
	case http.MethodPost:
		var payload protonLoginPayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		payload.Username = strings.TrimSpace(payload.Username)
		payload.Password = strings.TrimSpace(payload.Password)
		payload.TOTPSecret = strings.TrimSpace(payload.TOTPSecret)
		if payload.Username == "" || payload.Password == "" {
			http.Error(w, "username and password are required", http.StatusBadRequest)
			return
		}
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		if err := os.MkdirAll(filepath.Dir(s.protonLoginPath), 0o700); err != nil {
			http.Error(w, "failed to create login credential directory", http.StatusInternalServerError)
			return
		}
		if err := writeProtonLoginPayload(s.protonLoginPath, s.protonLoginKeyPath, payload); err != nil {
			http.Error(w, "failed to save proton login credentials", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": true, "path": s.protonLoginPath, "keyPath": s.protonLoginKeyPath, "encryptedAtRest": true})
	case http.MethodDelete:
		if err := os.Remove(s.protonLoginPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "failed to remove proton login credentials", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func readProtonLoginPayload(path, keyPath string) (protonLoginPayload, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return protonLoginPayload{}, false, nil
		}
		return protonLoginPayload{}, false, err
	}

	plain, err := decryptProtonLoginPayload(b, keyPath)
	if err != nil {
		return protonLoginPayload{}, false, err
	}

	var payload protonLoginPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return protonLoginPayload{}, false, err
	}
	payload.Username = strings.TrimSpace(payload.Username)
	payload.TOTPSecret = strings.TrimSpace(payload.TOTPSecret)
	return payload, true, nil
}

type protonLoginValidateRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totpSecret"`
}

type protonLoginValidationAttempt struct {
	AppVersion   string `json:"appVersion"`
	OK           bool   `json:"ok"`
	Stage        string `json:"stage,omitempty"`
	Error        string `json:"error,omitempty"`
	RequiresTOTP bool   `json:"requiresTOTP,omitempty"`
}

func (s *Server) handleProtonLoginValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req protonLoginValidateRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)

	username := strings.TrimSpace(req.Username)
	password := strings.TrimSpace(req.Password)
	totpSecret := strings.TrimSpace(req.TOTPSecret)

	if username == "" || password == "" {
		stored, exists, err := readProtonLoginPayload(s.protonLoginPath, s.protonLoginKeyPath)
		if err != nil {
			http.Error(w, "failed to load stored proton credentials", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "username and password are required (or store credentials first)", http.StatusBadRequest)
			return
		}
		if username == "" {
			username = strings.TrimSpace(stored.Username)
		}
		if password == "" {
			password = strings.TrimSpace(stored.Password)
		}
		if totpSecret == "" {
			totpSecret = strings.TrimSpace(stored.TOTPSecret)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	appVersions := protonLoginAppVersionsFromEnv()
	attempts := make([]protonLoginValidationAttempt, 0, len(appVersions))

	for _, appVersion := range appVersions {
		attempt := validateProtonLoginWithAppVersion(ctx, username, password, totpSecret, appVersion)
		attempts = append(attempts, attempt)
		if attempt.OK {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":           true,
				"requiresTOTP": attempt.RequiresTOTP,
				"appVersion":   attempt.AppVersion,
				"attempts":     attempts,
			})
			return
		}
	}

	last := attempts[len(attempts)-1]
	detailedErr := buildProtonLoginAttemptError(attempts)
	resp := map[string]any{
		"ok":         false,
		"error":      detailedErr,
		"stage":      last.Stage,
		"appVersion": last.AppVersion,
		"attempts":   attempts,
	}
	if last.RequiresTOTP {
		resp["requiresTOTP"] = true
	}
	writeJSON(w, http.StatusUnprocessableEntity, resp)
}

func validateProtonLoginWithAppVersion(ctx context.Context, username, password, totpSecret, appVersion string) protonLoginValidationAttempt {
	attempt := protonLoginValidationAttempt{AppVersion: strings.TrimSpace(appVersion)}
	mgr := protonapi.New(protonapi.WithAppVersion(attempt.AppVersion))

	client, auth, err := mgr.NewClientWithLogin(ctx, username, []byte(password))
	if err != nil {
		attempt.Stage = "login"
		attempt.Error = err.Error()
		return attempt
	}
	defer client.Close()

	needsTOTP := auth.TwoFA.Enabled&protonapi.HasTOTP != 0
	attempt.RequiresTOTP = needsTOTP
	if needsTOTP {
		if strings.TrimSpace(totpSecret) == "" {
			attempt.Stage = "2fa"
			attempt.Error = "TOTP required but totpSecret is missing"
			return attempt
		}
		code, err := totp.GenerateCode(strings.TrimSpace(totpSecret), time.Now())
		if err != nil {
			attempt.Stage = "2fa"
			attempt.Error = "failed to generate TOTP code"
			return attempt
		}
		if err := client.Auth2FA(ctx, protonapi.Auth2FAReq{TwoFactorCode: code}); err != nil {
			attempt.Stage = "2fa"
			attempt.Error = err.Error()
			return attempt
		}
	}

	if _, err := client.GetUser(ctx); err != nil {
		attempt.Stage = "user"
		attempt.Error = err.Error()
		return attempt
	}

	attempt.OK = true
	return attempt
}

func protonLoginAppVersionsFromEnv() []string {
	primary := strings.TrimSpace(os.Getenv("PROTON_LOGIN_APP_VERSION"))
	fallbackRaw := strings.TrimSpace(os.Getenv("PROTON_LOGIN_APP_VERSION_FALLBACKS"))

	out := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := strings.ToLower(v)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, v)
	}

	add(primary)
	for _, v := range strings.Split(fallbackRaw, ",") {
		add(v)
	}

	if len(out) == 0 {
		add("Other")
		add("web-mail@6.10.0.0")
	}

	return out
}

func buildProtonLoginAttemptError(attempts []protonLoginValidationAttempt) string {
	if len(attempts) == 0 {
		return "proton login validation failed"
	}
	last := attempts[len(attempts)-1]
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		label := strings.TrimSpace(a.AppVersion)
		if label == "" {
			label = "(empty)"
		}
		stage := strings.TrimSpace(a.Stage)
		if stage == "" {
			stage = "unknown"
		}
		errMsg := strings.TrimSpace(a.Error)
		if errMsg == "" {
			errMsg = "no error message"
		}
		parts = append(parts, fmt.Sprintf("%s/%s: %s", label, stage, errMsg))
	}

	base := strings.TrimSpace(last.Error)
	if base == "" {
		base = "proton login validation failed"
	}
	return base + " | attempts: " + strings.Join(parts, " ; ")
}

type encryptedProtonLoginPayload struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func writeProtonLoginPayload(path, keyPath string, payload protonLoginPayload) error {
	plain, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeEncryptedPayload(path, keyPath, plain)
}

func writeEncryptedPrivateSecret(path, keyPath string, payload []byte) error {
	return writeEncryptedPayload(path, keyPath, payload)
}

func writeEncryptedPayload(path, keyPath string, payload []byte) error {
	key, err := loadOrCreateEncryptionKey(keyPath)
	if err != nil {
		return err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}

	sealed := gcm.Seal(nil, nonce, payload, nil)
	env := encryptedProtonLoginPayload{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}

	return atomicWritePrivateFile(path, b)
}

func decryptProtonLoginPayload(raw []byte, keyPath string) ([]byte, error) {
	return decryptEncryptedPayload(raw, keyPath)
}

func decryptEncryptedPayload(raw []byte, keyPath string) ([]byte, error) {
	var env encryptedProtonLoginPayload
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != 1 || strings.TrimSpace(env.Nonce) == "" || strings.TrimSpace(env.Ciphertext) == "" {
		// Backward-compatibility with plaintext credentials.
		return raw, nil
	}

	key, err := loadOrCreateEncryptionKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

func loadOrCreateEncryptionKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if decErr != nil {
			return nil, decErr
		}
		if len(decoded) != 32 {
			return nil, errors.New("invalid proton login master key length")
		}
		return decoded, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	if err := atomicWritePrivateFile(path, encoded); err != nil {
		return nil, err
	}
	return key, nil
}

func isEncryptedPayloadFile(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var env encryptedProtonLoginPayload
	if err := json.Unmarshal(b, &env); err != nil {
		return false
	}
	return env.Version == 1 && strings.TrimSpace(env.Nonce) != "" && strings.TrimSpace(env.Ciphertext) != ""
}

type protonTokenFileDebug struct {
	Path                string   `json:"path"`
	Exists              bool     `json:"exists"`
	Readable            bool     `json:"readable"`
	Parseable           bool     `json:"parseable"`
	Size                int64    `json:"size"`
	ModifiedAt          string   `json:"modifiedAt,omitempty"`
	UpdatedAt           string   `json:"updatedAt,omitempty"`
	ClientID            string   `json:"clientId,omitempty"`
	UIDPresent          bool     `json:"uidPresent"`
	AccessTokenPresent  bool     `json:"accessTokenPresent"`
	RefreshTokenPresent bool     `json:"refreshTokenPresent"`
	TokenReady          bool     `json:"tokenReady"`
	CookieCount         int      `json:"cookieCount"`
	CookieNames         []string `json:"cookieNames,omitempty"`
	Error               string   `json:"error,omitempty"`
}

type protonRefreshDebug struct {
	Disabled       bool   `json:"disabled"`
	Reason         string `json:"reason,omitempty"`
	LastPersistAt  string `json:"lastPersistAt,omitempty"`
	LastPersistErr string `json:"lastPersistError,omitempty"`
	LastReloginAt  string `json:"lastReloginAt,omitempty"`
	LastReloginErr string `json:"lastReloginError,omitempty"`
}

func (s *Server) handleProtonTokenState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mainState := readProtonTokenFileDebug(s.protonAuthPath)
	snapshotState := readProtonTokenFileDebug(s.protonAuthPath + ".last-good")
	refreshState := protonRefreshDebug{}
	if dbg, ok := s.proton.(interface{ DebugAuthState() proton.AuthDebugInfo }); ok {
		info := dbg.DebugAuthState()
		refreshState.Disabled = info.RefreshDisabled
		refreshState.Reason = info.RefreshReason
		refreshState.LastPersistAt = info.LastPersistAt
		refreshState.LastPersistErr = info.LastPersistErr
		refreshState.LastReloginAt = info.LastReloginAt
		refreshState.LastReloginErr = info.LastReloginErr
	}

	recommendedSource := "none"
	if mainState.TokenReady {
		recommendedSource = "main"
	} else if snapshotState.TokenReady {
		recommendedSource = "snapshot"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"recommendedSource": recommendedSource,
		"main":              mainState,
		"snapshot":          snapshotState,
		"refresh":           refreshState,
	})
}

func readProtonTokenFileDebug(filePath string) protonTokenFileDebug {
	st := protonTokenFileDebug{Path: filePath}

	info, err := os.Stat(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st
		}
		st.Error = "stat failed"
		return st
	}

	st.Exists = true
	st.Size = info.Size()
	st.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)

	b, err := os.ReadFile(filePath)
	if err != nil {
		st.Error = "read failed"
		return st
	}
	st.Readable = true

	parsed := map[string]any{}
	if err := json.Unmarshal(b, &parsed); err != nil {
		st.Error = "parse failed"
		return st
	}
	st.Parseable = true

	uid := mapString(parsed, "uid")
	access := mapString(parsed, "accessToken")
	refresh := mapString(parsed, "refreshToken")
	st.UIDPresent = strings.TrimSpace(uid) != ""
	st.AccessTokenPresent = strings.TrimSpace(access) != ""
	st.RefreshTokenPresent = strings.TrimSpace(refresh) != ""
	st.TokenReady = st.UIDPresent && st.AccessTokenPresent && st.RefreshTokenPresent
	st.UpdatedAt = mapString(parsed, "updatedAt")
	st.ClientID = mapString(parsed, "clientID")
	st.CookieCount, st.CookieNames = extractCookieMeta(parsed)

	return st
}

func mapString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func extractCookieMeta(parsed map[string]any) (int, []string) {
	raw, ok := parsed["cookies"]
	if !ok {
		return 0, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return 0, nil
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(list))
	count := 0
	for _, item := range list {
		cookieMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		count++
		name := strings.TrimSpace(mapString(cookieMap, "name"))
		if name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return count, names
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
				fallback := strings.TrimSpace(llama.LoadTuningText())
				if fallback != "" {
					writeJSON(w, http.StatusOK, map[string]any{"content": fallback, "path": s.tuningPath})
					return
				}
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
		restartOk := true
		restartError := ""
		if err := restartLlamaProcess(r.Context()); err != nil {
			restartOk = false
			restartError = err.Error()
		} else {
			llama.ResetWarmupState()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": s.tuningPath, "restartOk": restartOk, "restartError": restartError})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLlamaAuth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(s.llamaAuthPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusOK, map[string]any{
					"exists":       false,
					"path":         s.llamaAuthPath,
					"localEnabled": strings.EqualFold(envOrDefault("LLAMA_LOCAL_ENABLED", "true"), "true"),
				})
				return
			}
			http.Error(w, "failed to read llama auth status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exists":       true,
			"path":         s.llamaAuthPath,
			"size":         info.Size(),
			"modifiedAt":   info.ModTime().UTC().Format(time.RFC3339),
			"localEnabled": strings.EqualFold(envOrDefault("LLAMA_LOCAL_ENABLED", "true"), "true"),
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
		if err := os.MkdirAll(filepath.Dir(s.llamaAuthPath), 0o755); err != nil {
			http.Error(w, "failed to create auth directory", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(s.llamaAuthPath, payload, 0o600); err != nil {
			http.Error(w, "failed to save auth file", http.StatusInternalServerError)
			return
		}
		if err := restartLlamaProcess(r.Context()); err != nil {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"ok":           true,
				"path":         s.llamaAuthPath,
				"filename":     header.Filename,
				"restartOk":    false,
				"restartError": err.Error(),
			})
			return
		}
		llama.ResetWarmupState()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"path":      s.llamaAuthPath,
			"filename":  header.Filename,
			"restartOk": true,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// protonSessionInjector is satisfied by *proton.APIClient via a type assertion.
// It is not part of the proton.Client interface to keep that interface minimal.
type protonSessionInjector interface {
	InjectSession(b proton.SessionBootstrap) error
}

func (s *Server) handleProtonAuthBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var b proton.SessionBootstrap
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(b.UID) == "" || strings.TrimSpace(b.AccessToken) == "" || strings.TrimSpace(b.RefreshToken) == "" {
		http.Error(w, "uid, accessToken, and refreshToken are required", http.StatusBadRequest)
		return
	}

	inj, ok := s.proton.(protonSessionInjector)
	if !ok {
		http.Error(w, "proton client does not support session injection", http.StatusNotImplemented)
		return
	}

	if err := inj.InjectSession(b); err != nil {
		http.Error(w, "failed to inject session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"uid":         strings.TrimSpace(b.UID),
		"cookieCount": len(b.Cookies),
	})
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
		uid, access, refresh, clientID, cookies, err := extractProtonTokensFromStorageState(payload)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("proton auth upload parse failed", "filename", header.Filename, "bytes", strconv.Itoa(len(payload)), "error", err.Error())
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
			return
		}

		unlock, lockErr := lockProtonAuthFileForValidation()
		if lockErr != nil {
			http.Error(w, "failed to lock proton auth store", http.StatusInternalServerError)
			return
		}
		defer unlock()

		validatedUID, validatedAccess, validatedRefresh, refreshPayloadComplete, missingRefreshFields, validateErr := validateAndRotateProtonAuth(r.Context(), uid, access, refresh, cookies)
		if validateErr != nil {
			if s.logger != nil {
				s.logger.Error("proton auth upload validation failed", "filename", header.Filename, "client_id", clientID, "cookie_count", strconv.Itoa(len(cookies)), "uid_present", strconv.FormatBool(strings.TrimSpace(uid) != ""), "error", validateErr.Error())
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "uploaded Proton session is not refreshable; please export a fresh mail auth file and try again"})
			return
		}
		if !refreshPayloadComplete && s.logger != nil {
			missing := strings.Join(missingRefreshFields, ",")
			if missing == "" {
				missing = "unknown"
			}
			s.logger.Info("proton auth upload validation succeeded with partial refresh payload", "filename", header.Filename, "client_id", clientID, "cookie_count", strconv.Itoa(len(cookies)), "missing_auth_fields", missing)
		}
		uid = validatedUID
		access = validatedAccess
		refresh = validatedRefresh

		if err := os.MkdirAll(filepath.Dir(s.protonAuthPath), 0o755); err != nil {
			http.Error(w, "failed to create proton auth directory", http.StatusInternalServerError)
			return
		}
		content, err := json.MarshalIndent(map[string]any{
			"uid":          uid,
			"accessToken":  access,
			"refreshToken": refresh,
			"source":       "llama-storage-state",
			"clientID":     clientID,
			"cookies":      cookies,
			"updatedAt":    time.Now().UTC().Format(time.RFC3339),
		}, "", "  ")
		if err != nil {
			http.Error(w, "failed to encode proton auth output", http.StatusInternalServerError)
			return
		}
		if err := atomicWritePrivateFile(s.protonAuthPath, content); err != nil {
			http.Error(w, "failed to finalize proton auth file", http.StatusInternalServerError)
			return
		}
		if err := atomicWritePrivateFile(protonTokenSnapshotPath(s.protonAuthPath), content); err != nil {
			http.Error(w, "failed to finalize proton auth snapshot", http.StatusInternalServerError)
			return
		}
		if s.logger != nil {
			s.logger.Info("proton auth upload persisted", "client_id", clientID, "cookies", strconv.Itoa(len(cookies)), "path", s.protonAuthPath)
		}

		llamaAuthUpdated := false
		llamaAuthError := ""
		if strings.TrimSpace(s.llamaAuthPath) != "" {
			if err := os.MkdirAll(filepath.Dir(s.llamaAuthPath), 0o755); err != nil {
				llamaAuthError = err.Error()
			} else if err := os.WriteFile(s.llamaAuthPath, payload, 0o600); err != nil {
				llamaAuthError = err.Error()
			} else {
				llamaAuthUpdated = true
			}
			if llamaAuthError != "" && s.logger != nil {
				s.logger.Error("llama auth mirror update failed during proton auth upload", "path", s.llamaAuthPath, "error", llamaAuthError)
			}
		}

		nextAction := "Proton tokens saved. Daemon restart has been queued to apply them."

		// Restart asynchronously so the upload endpoint always returns promptly.
		// A blocked supervisorctl call should not surface to the UI as a generic
		// network error ("Failed to fetch").
		go func() {
			restartCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			if err := restartDaemonProcess(restartCtx); err == nil {
				if s.logger != nil {
					s.logger.Info("daemon restart requested after proton auth upload", "method", "supervisorctl restart daemon")
				}
				return
			}

			// supervisorctl could not restart the daemon program. Rather than killing
			// PID 1 (which takes the whole container down and depends on an external
			// Docker restart policy to recover), signal the daemon process directly so
			// supervisord's autorestart respawns it. Applying new Proton tokens never
			// requires a full container restart.
			if signalErr := signalDaemonProcessRestart(); signalErr != nil {
				if s.logger != nil {
					s.logger.Error("daemon restart after proton auth update failed", "supervisorctl_error", err.Error(), "signal_error", signalErr.Error())
				}
			} else if s.logger != nil {
				s.logger.Info("daemon restart requested after proton auth upload", "method", "signal daemon process")
			}
		}()

		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":               true,
			"path":             s.protonAuthPath,
			"filename":         header.Filename,
			"conversionMethod": "cookie-extract",
			"llamaAuthPath":    s.llamaAuthPath,
			"llamaAuthUpdated": llamaAuthUpdated,
			"llamaAuthError":   llamaAuthError,
			"restartRequested": false,
			"nextAction":       nextAction,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleProtonPrivateKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keyInfo, keyErr := os.Stat(s.protonKeyPath)
		passInfo, passErr := os.Stat(s.protonPassPath)
		if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
			http.Error(w, "failed to read proton private key status", http.StatusInternalServerError)
			return
		}
		if passErr != nil && !errors.Is(passErr, os.ErrNotExist) {
			http.Error(w, "failed to read proton private key password status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"keyExists":            keyErr == nil,
			"keyPath":              s.protonKeyPath,
			"keyEncryptionKeyPath": s.protonKeyEncKeyPath,
			"keySize":              fileSizeOrZero(keyInfo),
			"keyModifiedAt":        fileTimeOrEmpty(keyInfo),
			"passwordExists":       passErr == nil,
			"passwordPath":         s.protonPassPath,
			"passwordModifiedAt":   fileTimeOrEmpty(passInfo),
			"encryptedAtRest":      isEncryptedPayloadFile(s.protonKeyPath) && isEncryptedPayloadFile(s.protonPassPath),
			"decryptReady":         keyErr == nil && passErr == nil,
		})
	case http.MethodPost:
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			http.Error(w, "invalid multipart request", http.StatusBadRequest)
			return
		}

		password := strings.TrimSpace(r.FormValue("password"))
		file, header, err := r.FormFile("keyFile")
		hasFile := err == nil
		if err != nil && !errors.Is(err, http.ErrMissingFile) {
			http.Error(w, "failed to read private key file", http.StatusBadRequest)
			return
		}
		if !hasFile && password == "" {
			http.Error(w, "keyFile or password is required", http.StatusBadRequest)
			return
		}
		if hasFile {
			defer file.Close()
		}

		if hasFile {
			payload, readErr := io.ReadAll(io.LimitReader(file, 16<<20))
			if readErr != nil {
				http.Error(w, "failed to read private key file", http.StatusBadRequest)
				return
			}
			if len(strings.TrimSpace(string(payload))) == 0 {
				http.Error(w, "private key file is empty", http.StatusBadRequest)
				return
			}
			if !strings.Contains(string(payload), "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
				http.Error(w, "private key file is not an armored Proton private key", http.StatusBadRequest)
				return
			}
			if err := writeEncryptedPrivateSecret(s.protonKeyPath, s.protonKeyEncKeyPath, payload); err != nil {
				http.Error(w, "failed to save private key file", http.StatusInternalServerError)
				return
			}
		}

		if password != "" {
			if err := writeEncryptedPrivateSecret(s.protonPassPath, s.protonKeyEncKeyPath, []byte(password)); err != nil {
				http.Error(w, "failed to save private key password", http.StatusInternalServerError)
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                   true,
			"keyPath":              s.protonKeyPath,
			"keyEncryptionKeyPath": s.protonKeyEncKeyPath,
			"passwordPath":         s.protonPassPath,
			"filename":             uploadedFilename(header),
			"passwordUpdated":      password != "",
			"encryptedAtRest":      true,
			"decryptReady":         protonPrivateKeyReady(s.protonKeyPath, s.protonPassPath),
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
	logDir := envOrDefault("LOG_DIR", "/llama_lab/logs")
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
	logDir := envOrDefault("LOG_DIR", "/llama_lab/logs")
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
	if req.Username != admin["ADMIN_USER"] || !verifyAdminPassword(admin, req.Password) {
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
	http.SetCookie(w, &http.Cookie{Name: "llama_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true")})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie("llama_session")
	if err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "llama_session", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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
	if !mustChange && !verifyAdminPassword(admin, req.OldPassword) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if mustChange && strings.TrimSpace(req.OldPassword) != "" && !verifyAdminPassword(admin, req.OldPassword) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	hash, err := hashAdminPassword(req.NewPassword)
	if err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	admin["ADMIN_PASS_HASH"] = hash
	delete(admin, "ADMIN_PASS")
	admin["MUST_CHANGE_PASSWORD"] = "false"
	if err := writeAdminEnv(s.adminPath, admin); err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLlamaTest(w http.ResponseWriter, r *http.Request) {
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

	baseURL := strings.TrimSpace(cfg.Llama.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LLAMA_BASE_URL"))
	}
	if baseURL == "" {
		http.Error(w, "llama base url is not configured", http.StatusBadRequest)
		return
	}

	path := strings.TrimSpace(cfg.Llama.ClassifyPath)
	if path == "" {
		path = "/"
	}
	apiKey := strings.TrimSpace(cfg.Llama.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("LLAMA_API_KEY"))
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "Email Address: test@example.com  Subject Line: Llama connectivity test Return only the label Updates"
	}

	allowed := cfg.Labels.Allowlist
	if len(allowed) == 0 {
		allowed = []string{"Questionable", "Important"}
	}

	tuning := llama.LoadTuningText()
	client := llama.NewHTTPClient(baseURL, apiKey, path, tuning, 120*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	result, err := client.Classify(ctx, allowed, "", "", prompt)
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
	frontendDir := envOrDefault("FRONTEND_DIR", "/opt/llama-lab/frontend")
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
	cookie, err := r.Cookie("llama_session")
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
	candidates := []string{"/llama_lab/config/TUNING.md", "TUNING.md", "/opt/llama-lab/TUNING.md"}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/llama_lab/config/TUNING.md"
}

func restartLlamaProcess(ctx context.Context) error {
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "supervisorctl", args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	out, err := run("-c", "/etc/supervisord.conf", "restart", "llama")
	if err == nil {
		llama.ResetWarmupState()
		return nil
	}

	msg := out
	if msg == "" {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "not running") || strings.Contains(lower, "spawn error") || strings.Contains(lower, "fatal") {
		startOut, startErr := run("-c", "/etc/supervisord.conf", "start", "llama")
		if startErr == nil {
			llama.ResetWarmupState()
			return nil
		}
		if strings.TrimSpace(startOut) != "" {
			msg = msg + "; start attempt: " + strings.TrimSpace(startOut)
		}
	}

	return fmt.Errorf("restart llama: %s", msg)
}

func restartDaemonProcess(ctx context.Context) error {
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "supervisorctl", args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	out, err := run("-c", "/etc/supervisord.conf", "restart", "daemon")
	if err == nil {
		return nil
	}

	msg := out
	if msg == "" {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "not running") || strings.Contains(lower, "spawn error") || strings.Contains(lower, "fatal") {
		startOut, startErr := run("-c", "/etc/supervisord.conf", "start", "daemon")
		if startErr == nil {
			return nil
		}
		if strings.TrimSpace(startOut) != "" {
			msg = msg + "; start attempt: " + strings.TrimSpace(startOut)
		}
	}

	return fmt.Errorf("restart daemon: %s", msg)
}

// signalDaemonProcessRestart finds the running `llama-lab --mode daemon` process
// and sends it SIGTERM. The daemon program is configured with autorestart=true
// in supervisord, so supervisord respawns it with the freshly written tokens.
// This is used as a fallback when supervisorctl is unavailable, avoiding a full
// container shutdown.
func signalDaemonProcessRestart() error {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("read /proc: %w", err)
	}

	signaled := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self || pid == 1 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		// /proc/<pid>/cmdline is NUL-separated.
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if !strings.Contains(cmdline, "llama-lab") {
			continue
		}
		if !strings.Contains(cmdline, "--mode") || !strings.Contains(cmdline, "daemon") {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			signaled++
		}
	}

	if signaled == 0 {
		return errors.New("daemon process not found")
	}
	return nil
}

type storageStateCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

type storageState struct {
	Cookies []storageStateCookie `json:"cookies"`
}

// protonCookie is the persisted representation of a Proton web-session cookie.
// These cookies (Session-Id, AUTH-<uid>, REFRESH-<uid>) are required to refresh
// a cookie-auth web session via /auth/v4/refresh; without them Proton rejects
// the refresh with 422 "Invalid input" once the bearer access token expires.
type protonCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

type refreshCookiePayload struct {
	ClientID     string `json:"ClientID"`
	RefreshToken string `json:"RefreshToken"`
	UID          string `json:"UID"`
}

func validateAndRotateProtonAuth(ctx context.Context, uid, access, refresh string, cookies []protonCookie) (string, string, string, bool, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	jar, err := protonCookieJar(cookies)
	if err != nil {
		return "", "", "", false, nil, err
	}

	manager := protonapi.New(
		protonapi.WithAppVersion("web-mail@6.10.0.0"),
		protonapi.WithCookieJar(jar),
	)

	_, auth, err := manager.NewClientWithRefresh(ctx, strings.TrimSpace(uid), strings.TrimSpace(refresh))
	if err != nil {
		return "", "", "", false, nil, err
	}

	resolvedUID := firstNonEmpty(strings.TrimSpace(auth.UID), strings.TrimSpace(uid))
	resolvedAccess := firstNonEmpty(strings.TrimSpace(auth.AccessToken), strings.TrimSpace(access))
	resolvedRefresh := firstNonEmpty(strings.TrimSpace(auth.RefreshToken), strings.TrimSpace(refresh))
	missingFields := missingAuthFields(auth)
	payloadComplete := len(missingFields) == 0

	if resolvedUID == "" || resolvedAccess == "" || resolvedRefresh == "" {
		return "", "", "", false, missingFields, errors.New("proton refresh validation returned unusable auth data")
	}

	return resolvedUID, resolvedAccess, resolvedRefresh, payloadComplete, missingFields, nil
}

func lockProtonAuthFileForValidation() (func(), error) {
	path := envOrDefault("PROTON_AUTH_FILE", "/llama_lab/config/proton-auth.json")
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, err
	}

	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

func missingAuthFields(auth protonapi.Auth) []string {
	missing := make([]string, 0, 3)
	if strings.TrimSpace(auth.UID) == "" {
		missing = append(missing, "uid")
	}
	if strings.TrimSpace(auth.AccessToken) == "" {
		missing = append(missing, "accessToken")
	}
	if strings.TrimSpace(auth.RefreshToken) == "" {
		missing = append(missing, "refreshToken")
	}
	return missing
}

func firstNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return strings.TrimSpace(primary)
	}
	return strings.TrimSpace(fallback)
}

func protonCookieJar(cookies []protonCookie) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	byHost := map[string][]*http.Cookie{}
	for _, c := range cookies {
		name := strings.TrimSpace(c.Name)
		value := strings.TrimSpace(c.Value)
		domain := strings.TrimSpace(c.Domain)
		if name == "" || value == "" || domain == "" {
			continue
		}
		host := strings.TrimPrefix(domain, ".")
		if host == "" {
			continue
		}
		byHost[host] = append(byHost[host], &http.Cookie{
			Name:   name,
			Value:  value,
			Domain: domain,
			Path:   "/",
		})
	}

	if len(byHost) == 0 {
		return nil, errors.New("storageState auth file did not include usable proton.me cookies")
	}

	for host, hostCookies := range byHost {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		jar.SetCookies(u, hostCookies)
	}

	return jar, nil
}

func extractProtonTokensFromStorageState(payload []byte) (string, string, string, string, []protonCookie, error) {
	var state storageState
	if err := json.Unmarshal(payload, &state); err != nil {
		return "", "", "", "", nil, errors.New("storageState auth file is not valid json")
	}
	if len(state.Cookies) == 0 {
		return "", "", "", "", nil, errors.New("storageState auth file has no cookies")
	}

	type refreshData struct {
		refreshToken string
		clientID     string
		domain       string
	}
	accessByUID := map[string]string{}
	refreshByUID := map[string]refreshData{}

	// Collect every proton.me cookie so the daemon can rebuild the full web
	// session context (Session-Id + AUTH/REFRESH forks) when refreshing tokens.
	sessionCookies := make([]protonCookie, 0, len(state.Cookies))

	for _, cookie := range state.Cookies {
		if strings.Contains(cookie.Domain, "proton.me") && strings.TrimSpace(cookie.Value) != "" {
			sessionCookies = append(sessionCookies, protonCookie{
				Name:   cookie.Name,
				Value:  cookie.Value,
				Domain: cookie.Domain,
				Path:   cookie.Path,
			})
		}
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
			// Each session fork (mail, account, …) has its own uid, so there is
			// normally one REFRESH cookie per uid. Keep the first one seen and only
			// replace it with a duplicate served from the app's own subdomain.
			if current, ok := refreshByUID[uid]; ok {
				if !strings.Contains(cookie.Domain, "proton.me") || strings.Contains(current.domain, "proton.me") {
					continue
				}
			}
			refreshByUID[uid] = refreshData{refreshToken: refresh, clientID: strings.TrimSpace(parsed.ClientID), domain: cookie.Domain}
		}
	}

	// Prefer the web-mail session fork. Its refresh token's ClientID matches the
	// web-mail App-Version the poller sends to the Proton API, so refreshing the
	// access token after it expires (~24h) succeeds instead of returning
	// 422 "Invalid input" (which happens when a WebAccount refresh token is
	// presented with a web-mail App-Version).
	selectedUID := ""
	selectedClientID := ""
	selectPair := func(match func(refreshData) bool) bool {
		for uid, refresh := range refreshByUID {
			if _, ok := accessByUID[uid]; !ok {
				continue
			}
			if match(refresh) {
				selectedUID = uid
				selectedClientID = refresh.clientID
				return true
			}
		}
		return false
	}
	// 1) Exact web-mail client, 2) any mail client, 3) any available pair.
	if !selectPair(func(r refreshData) bool { return strings.EqualFold(r.clientID, "WebMail") }) {
		if !selectPair(func(r refreshData) bool { return strings.Contains(strings.ToLower(r.clientID), "mail") }) {
			selectPair(func(r refreshData) bool { return true })
		}
	}
	if selectedUID == "" {
		return "", "", "", "", nil, errors.New("could not extract matching AUTH/REFRESH token pair from storageState cookies")
	}
	refresh := refreshByUID[selectedUID].refreshToken
	access := accessByUID[selectedUID]
	if strings.TrimSpace(refresh) == "" || strings.TrimSpace(access) == "" {
		return "", "", "", "", nil, errors.New("extracted proton token pair is incomplete")
	}
	return selectedUID, access, refresh, selectedClientID, sessionCookies, nil
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
	content := fmt.Sprintf("ADMIN_USER=%s\n", kv["ADMIN_USER"])
	if hash := strings.TrimSpace(kv["ADMIN_PASS_HASH"]); hash != "" {
		content += fmt.Sprintf("ADMIN_PASS_HASH=%s\n", hash)
	} else {
		content += fmt.Sprintf("ADMIN_PASS=%s\n", kv["ADMIN_PASS"])
	}
	content += fmt.Sprintf("MUST_CHANGE_PASSWORD=%s\n", kv["MUST_CHANGE_PASSWORD"])
	return os.WriteFile(path, []byte(content), 0o600)
}

func verifyAdminPassword(admin map[string]string, candidate string) bool {
	hash := strings.TrimSpace(admin["ADMIN_PASS_HASH"])
	if hash != "" {
		return verifyScryptHash(hash, candidate)
	}
	legacy := admin["ADMIN_PASS"]
	if legacy == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(legacy), []byte(candidate)) == 1
}

func hashAdminPassword(password string) (string, error) {
	const (
		n      = 16384
		r      = 8
		p      = 1
		keyLen = 32
	)
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := scrypt.Key([]byte(password), salt, n, r, p, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"scrypt$%d$%d$%d$%s$%s",
		n,
		r,
		p,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(hash),
	), nil
}

func verifyScryptHash(encoded, candidate string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	r, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	p, err := strconv.Atoi(parts[3])
	if err != nil {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	if len(expected) == 0 {
		return false
	}
	derived, err := scrypt.Key([]byte(candidate), salt, n, r, p, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(derived, expected) == 1
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func atomicWritePrivateFile(path string, payload []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func protonTokenSnapshotPath(path string) string {
	return path + ".last-good"
}

func fileSizeOrZero(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

func fileTimeOrEmpty(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return info.ModTime().UTC().Format(time.RFC3339)
}

func uploadedFilename(header *multipart.FileHeader) string {
	if header == nil {
		return ""
	}
	return header.Filename
}

func protonPrivateKeyReady(keyPath, passwordPath string) bool {
	if _, err := os.Stat(keyPath); err != nil {
		return false
	}
	if _, err := os.Stat(passwordPath); err != nil {
		return false
	}
	return true
}
