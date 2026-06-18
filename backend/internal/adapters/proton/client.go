package proton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	protonapi "github.com/ProtonMail/go-proton-api"
	pgpcrypto "github.com/ProtonMail/gopenpgp/v2/crypto"
)

type Message struct {
	ID      string
	Subject string
	Sender  string
	Body    string
}

type Client interface {
	ListUnreadInbox(ctx context.Context, sinceCheckpoint string) ([]Message, string, error)
	ListLabels(ctx context.Context) ([]string, error)
	EnsureLabel(ctx context.Context, label string) error
	ApplyLabel(ctx context.Context, messageID, label string) error
}

// proactiveRefreshInterval is the minimum time between proactive token
// refreshes. Proton refresh tokens are single-use; calling the endpoint too
// frequently burns through the rotation chain and causes 400 errors.
const proactiveRefreshInterval = 1 * time.Hour

type APIClient struct {
	mu             sync.Mutex
	mgr            *protonapi.Manager
	client         *protonapi.Client
	labelByKey     map[string]string
	tokenFileMTime time.Time
	host           string
	versions       []string
	versionIdx     int
	skipRefresh    bool
	lastRefreshAt  time.Time
	nextRefreshAt  time.Time
	jar            http.CookieJar
	cookieMeta     []protonCookie
}

type tokenFile struct {
	UID          string `json:"uid"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// protonCookie mirrors the persisted web-session cookies (Session-Id,
// AUTH-<uid>, REFRESH-<uid>). They are replayed into a cookie jar so that
// /auth/v4/refresh succeeds for cookie-auth web sessions; a body-only refresh
// token without these cookies is rejected by Proton with 422 "Invalid input".
type protonCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

func NewAPIClientFromEnv() *APIClient {
	host := strings.TrimSpace(os.Getenv("PROTON_API_HOST"))
	versions := protonAppVersionsFromEnv()
	modTime := readTokenFileModTime()
	client := &APIClient{
		host:           host,
		versions:       versions,
		versionIdx:     0,
		labelByKey:     map[string]string{},
		tokenFileMTime: modTime,
	}
	client.cookieMeta = loadProtonCookies()
	client.jar = buildCookieJar(client.cookieMeta)
	client.mgr = newManager(host, client.currentVersion(), client.jar)
	return client
}

// refreshClient performs a proactive token refresh before each poll. It replaces
// the cached client with a new one carrying fresh credentials and persists the
// new tokens to disk so a restart always has valid credentials.
func (c *APIClient) refreshClient(ctx context.Context) error {
	uid, _, ref, err := readTokenFile()
	if err != nil {
		return err
	}

	pc, auth, err := c.mgr.NewClientWithRefresh(ctx, uid, ref)
	if err != nil {
		return err
	}

	// Persist the freshly issued tokens immediately.
	_ = writeTokenFile(auth.UID, auth.AccessToken, auth.RefreshToken)
	c.persistRotatedCookies()

	c.mu.Lock()
	c.lastRefreshAt = time.Now()
	c.nextRefreshAt = time.Now().Add(proactiveRefreshInterval)
	c.mu.Unlock()

	pc.AddAuthHandler(func(a protonapi.Auth) {
		_ = writeTokenFile(a.UID, a.AccessToken, a.RefreshToken)
		c.persistRotatedCookies()
		c.mu.Lock()
		c.tokenFileMTime = readTokenFileModTime()
		c.mu.Unlock()
	})
	// Advance tokenFileMTime so reloadClientOnTokenFileChange does not re-trigger
	// on the mtime change we just created, which would null the client and
	// immediately force another refresh, burning the just-issued token.
	c.mu.Lock()
	c.tokenFileMTime = readTokenFileModTime()
	c.mu.Unlock()

	pc.AddDeauthHandler(func() {
		c.mu.Lock()
		c.client = nil
		c.mu.Unlock()
	})

	c.mu.Lock()
	c.client = pc
	c.mu.Unlock()

	return nil
}

func (c *APIClient) ListUnreadInbox(ctx context.Context, sinceCheckpoint string) ([]Message, string, error) {
	// Pick up new auth uploads without requiring a daemon/server restart.
	c.reloadClientOnTokenFileChange()

	// Proactive refresh: best-effort. A fresh browser-extracted token may not
	// be refreshable via the API client (different ClientID / App-Version), so
	// fall back to using stored credentials directly on failure rather than
	// blocking the entire fetch.
	if !c.shouldSkipRefresh() {
		if err := c.refreshClient(ctx); err != nil {
			log.Printf("proton auth: proactive refresh failed; falling back to token-file client: %v", err)
			// Back off for one interval on any error so we do not burn through
			// single-use Proton refresh tokens (400) or hit version mismatches (422).
			c.mu.Lock()
			c.nextRefreshAt = time.Now().Add(proactiveRefreshInterval)
			c.mu.Unlock()
			// Always rebuild from stored disk credentials rather than reusing the
			// in-memory client – its access token may be near or past expiry, which
			// is likely what triggered the refresh failure in the first place.
			// Note: browser-extracted tokens legitimately return 422 from the refresh
			// endpoint (different ClientID), so we never treat refresh errors as fatal.
			c.mu.Lock()
			uid, acc, ref, tokenErr := readTokenFile()
			if tokenErr == nil {
				log.Printf("proton auth: rebuilt client from token file after refresh failure; uid_present=%t access_present=%t refresh_present=%t", strings.TrimSpace(uid) != "", strings.TrimSpace(acc) != "", strings.TrimSpace(ref) != "")
				pc := c.mgr.NewClient(uid, acc, ref)
				pc.AddAuthHandler(func(a protonapi.Auth) {
					_ = writeTokenFile(a.UID, a.AccessToken, a.RefreshToken)
					c.persistRotatedCookies()
				})
				pc.AddDeauthHandler(func() {
					c.mu.Lock()
					c.client = nil
					c.mu.Unlock()
				})
				c.client = pc
			} else if c.client == nil {
				c.mu.Unlock()
				return nil, "", fmt.Errorf("fetch unread inbox failed: proactive refresh failed (%v) and token file reload failed (%w)", err, tokenErr)
			}
			c.mu.Unlock()
		}
	}

	maxAttempts := len(c.versions)
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		messages, checkpoint, err := c.listUnreadInboxOnce(ctx, sinceCheckpoint)
		if err == nil {
			return messages, checkpoint, nil
		}
		log.Printf("proton fetch: listUnreadInbox attempt failed; attempt=%d/%d version=%q error=%v", attempt+1, maxAttempts, c.currentVersion(), err)
		lastErr = err
		if !c.rotateVersionOnOutOfDate(err) {
			break
		}
	}

	return nil, "", fmt.Errorf("fetch unread inbox failed after %d attempt(s): %w", maxAttempts, lastErr)
}

func readTokenFileModTime() time.Time {
	info, err := os.Stat(tokenFilePath())
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

func (c *APIClient) reloadClientOnTokenFileChange() {
	modTime := readTokenFileModTime()
	if modTime.IsZero() {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !modTime.After(c.tokenFileMTime) {
		return
	}

	// Only swap clients when the updated file actually has usable token fields.
	// This prevents a malformed/partial write from dropping a currently healthy
	// in-memory client and causing avoidable auth failures mid-run.
	if _, _, _, err := readTokenFile(); err != nil {
		log.Printf("proton auth: token file changed but reload skipped because token fields are invalid: %v", err)
		return
	}

	cookies := loadProtonCookies()
	jar := buildCookieJar(cookies)
	version := ""
	if len(c.versions) > 0 {
		if c.versionIdx < 0 || c.versionIdx >= len(c.versions) {
			c.versionIdx = 0
		}
		version = c.versions[c.versionIdx]
	}

	// Force a clean client rebuild from the latest token file on next ensureClient.
	c.tokenFileMTime = modTime
	c.cookieMeta = cookies
	c.jar = jar
	c.mgr = newManager(c.host, version, c.jar)
	c.client = nil
	c.labelByKey = map[string]string{}
	c.skipRefresh = false
	c.nextRefreshAt = time.Time{}
}

func (c *APIClient) listUnreadInboxOnce(ctx context.Context, sinceCheckpoint string) ([]Message, string, error) {
	pc, err := c.ensureClient(ctx)
	if err != nil {
		return nil, "", err
	}

	currentCheckpoint := sinceCheckpoint
	if sinceCheckpoint == "" {
		ids, err := pc.GetMessageIDs(ctx, "")
		if err != nil {
			return nil, "", err
		}
		if len(ids) == 0 {
			return []Message{}, "", nil
		}
		currentCheckpoint = ""
	}

	ids, err := pc.GetMessageIDs(ctx, currentCheckpoint)
	if err != nil {
		return nil, "", err
	}
	if len(ids) == 0 {
		return []Message{}, currentCheckpoint, nil
	}

	out := make([]Message, 0, len(ids))
	fallbackUnreadInbox := make([]Message, 0, len(ids))
	decrypter, decryptConfigured, err := loadMessageDecrypter()
	if err != nil {
		return nil, "", err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		full, err := pc.GetMessage(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if !bool(full.Unread) || !containsLabel(full.LabelIDs, protonapi.InboxLabel) {
			continue
		}
		sender := ""
		if full.Sender != nil {
			sender = full.Sender.Address
		}
		msg := Message{
			ID:      full.ID,
			Subject: full.Subject,
			Sender:  sender,
			Body:    htmlToText(full.Body),
		}
		if isPGPArmored(full.Body) {
			if decryptConfigured {
				decrypted, err := decrypter.Decrypt(full.Body)
				if err != nil {
					// Keep the sweep alive if one message cannot be decrypted with the
					// currently uploaded secret key. Subject/sender are still usable for
					// classification, so a single bad body should not stop the inbox pass.
					msg.Body = ""
				} else {
					msg.Body = htmlToText(decrypted)
				}
			} else {
				// Without a configured private key, avoid sending raw ciphertext to Llama.
				msg.Body = ""
			}
		}
		fallbackUnreadInbox = append(fallbackUnreadInbox, msg)
		if hasUserLabel(full.LabelIDs) {
			continue
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		out = fallbackUnreadInbox
	}
	return out, ids[len(ids)-1], nil
}

func hasUserLabel(labelIDs []string) bool {
	for _, labelID := range labelIDs {
		if !isSystemLabel(labelID) {
			return true
		}
	}
	return false
}

func isSystemLabel(labelID string) bool {
	switch labelID {
	case protonapi.InboxLabel,
		protonapi.AllSentLabel,
		protonapi.TrashLabel,
		protonapi.SpamLabel,
		protonapi.AllMailLabel,
		protonapi.ArchiveLabel,
		protonapi.SentLabel,
		protonapi.DraftsLabel,
		protonapi.AllDraftsLabel,
		protonapi.StarredLabel,
		protonapi.OutboxLabel,
		protonapi.AllScheduledLabel:
		return true
	default:
		return false
	}
}

func protonAppVersionsFromEnv() []string {
	primary := strings.TrimSpace(os.Getenv("PROTON_APP_VERSION"))
	fallbackRaw := strings.TrimSpace(os.Getenv("PROTON_APP_VERSION_FALLBACKS"))

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
		// Try newer web-mail versions first, then fallback to legacy.
		add("web-mail@6.10.0.0")
		add("web-mail@6.0.0.0")
		add("web-mail@5.0.0.0")
	}

	return out
}

func newManager(host, appVersion string, jar http.CookieJar) *protonapi.Manager {
	opts := []protonapi.Option{}
	if strings.TrimSpace(appVersion) != "" {
		opts = append(opts, protonapi.WithAppVersion(strings.TrimSpace(appVersion)))
	}
	if strings.TrimSpace(host) != "" {
		opts = append(opts, protonapi.WithHostURL(strings.TrimSpace(host)))
	}
	if jar != nil {
		opts = append(opts, protonapi.WithCookieJar(jar))
	}
	return protonapi.New(opts...)
}

// loadProtonCookies reads the persisted web-session cookies from the proton auth
// file. Returns nil when the file has no cookies (older auth uploads), in which
// case the manager runs without a cookie jar exactly as before.
func loadProtonCookies() []protonCookie {
	b, err := os.ReadFile(tokenFilePath())
	if err != nil {
		return nil
	}
	var parsed struct {
		Cookies []protonCookie `json:"cookies"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil
	}
	return parsed.Cookies
}

// buildCookieJar seeds an in-memory cookie jar with the persisted Proton
// web-session cookies. Cookie paths are normalized to "/" so they are always
// sent to the refresh endpoint (the original REFRESH cookie path is scoped to
// /api/auth/refresh, which would otherwise exclude it from /api/auth/v4/refresh).
// The jar is updated automatically by Set-Cookie responses as tokens rotate.
func buildCookieJar(cookies []protonCookie) http.CookieJar {
	valid := make([]protonCookie, 0, len(cookies))
	for _, c := range cookies {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Value) == "" || strings.TrimSpace(c.Domain) == "" {
			continue
		}
		valid = append(valid, c)
	}
	if len(valid) == 0 {
		return nil
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}

	byHost := map[string][]*http.Cookie{}
	for _, c := range valid {
		host := strings.TrimPrefix(c.Domain, ".")
		byHost[host] = append(byHost[host], &http.Cookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: c.Domain,
			Path:   "/",
		})
	}
	for host, hc := range byHost {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		jar.SetCookies(u, hc)
	}
	return jar
}

// persistRotatedCookies snapshots the jar's current cookie values (refreshed by
// Set-Cookie responses as tokens rotate) and writes them back to the auth file
// so a daemon restart resumes with valid session cookies rather than the stale
// originals from the last upload.
func (c *APIClient) persistRotatedCookies() {
	c.mu.Lock()
	jar := c.jar
	meta := c.cookieMeta
	if jar == nil || len(meta) == 0 {
		c.mu.Unlock()
		return
	}

	latest := map[string]string{}
	hosts := map[string]bool{}
	for _, m := range meta {
		hosts[strings.TrimPrefix(m.Domain, ".")] = true
	}
	for host := range hosts {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		for _, ck := range jar.Cookies(u) {
			latest[ck.Name] = ck.Value
		}
	}

	updated := make([]protonCookie, 0, len(meta))
	changed := false
	for _, m := range meta {
		if v, ok := latest[m.Name]; ok && v != "" && v != m.Value {
			m.Value = v
			changed = true
		}
		updated = append(updated, m)
	}
	if !changed {
		c.mu.Unlock()
		return
	}
	c.cookieMeta = updated
	c.mu.Unlock()

	_ = writeCookieFile(updated)
}

func writeCookieFile(cookies []protonCookie) error {
	// Never create/overwrite the auth file with cookies alone.
	// If token fields are unavailable, skip persistence and keep runtime alive.
	if _, _, _, err := readTokenFile(); err != nil {
		return err
	}
	return updateTokenFile(func(existing map[string]any) {
		existing["cookies"] = cookies
		existing["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	})
}

func isOutOfDateError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "422") && strings.Contains(msg, "out of date") {
		return true
	}
	if strings.Contains(msg, "refresh the page") && strings.Contains(msg, "out of date") {
		return true
	}
	return false
}

func isExpectedRefreshInvalidInputError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if !strings.Contains(msg, "422") {
		return false
	}
	if !strings.Contains(msg, "invalid input") {
		return false
	}
	if strings.Contains(msg, "refresh") {
		return true
	}
	if strings.Contains(msg, "grant_type") {
		return true
	}
	return false
}

func (c *APIClient) shouldSkipRefresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.skipRefresh {
		return true
	}
	return time.Now().Before(c.nextRefreshAt)
}

func (c *APIClient) disableProactiveRefresh() {
	c.mu.Lock()
	c.skipRefresh = true
	c.mu.Unlock()
}

func (c *APIClient) currentVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.versions) == 0 {
		return ""
	}
	if c.versionIdx < 0 || c.versionIdx >= len(c.versions) {
		c.versionIdx = 0
	}
	return c.versions[c.versionIdx]
}

func (c *APIClient) rotateVersionOnOutOfDate(err error) bool {
	if !isOutOfDateError(err) {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.versions) == 0 || c.versionIdx >= len(c.versions)-1 {
		return false
	}

	c.versionIdx++
	c.mgr = newManager(c.host, c.versions[c.versionIdx], c.jar)
	c.client = nil
	c.labelByKey = map[string]string{}
	return true
}

func (c *APIClient) EnsureLabel(ctx context.Context, label string) error {
	_, err := c.ensureLabelID(ctx, label)
	return err
}

func (c *APIClient) ListLabels(ctx context.Context) ([]string, error) {
	pc, err := c.ensureClient(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := pc.GetLabels(ctx, protonapi.LabelTypeLabel)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(labels))
	seen := map[string]bool{}
	for _, label := range labels {
		name := strings.TrimSpace(label.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out, nil
}

func (c *APIClient) ApplyLabel(ctx context.Context, messageID, label string) error {
	pc, err := c.ensureClient(ctx)
	if err != nil {
		return err
	}
	labelID, err := c.ensureLabelID(ctx, label)
	if err != nil {
		return err
	}
	return pc.LabelMessages(ctx, []string{messageID}, labelID)
}

func (c *APIClient) ensureClient(ctx context.Context) (*protonapi.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}

	uid, acc, ref, err := readTokenFile()
	if err != nil {
		return nil, err
	}
	if uid != "" && acc != "" && ref != "" {
		pc := c.mgr.NewClient(uid, acc, ref)
		// Persist refreshed tokens to disk so a restart always has valid credentials.
		pc.AddAuthHandler(func(auth protonapi.Auth) {
			_ = writeTokenFile(auth.UID, auth.AccessToken, auth.RefreshToken)
			c.persistRotatedCookies()
		})
		// Clear the cached client on de-auth (422) so ensureClient re-reads the
		// token file on the next poll rather than permanently returning a dead client.
		pc.AddDeauthHandler(func() {
			c.mu.Lock()
			c.client = nil
			c.mu.Unlock()
		})
		c.client = pc
		return c.client, nil
	}

	return nil, errors.New("missing proton token credentials in token file")
}

func tokenFilePath() string {
	if path := strings.TrimSpace(os.Getenv("PROTON_AUTH_FILE")); path != "" {
		return path
	}
	return "/llama_lab/config/proton-auth.json"
}

func tokenSnapshotFilePath(path string) string {
	return path + ".last-good"
}

func hasTokenFields(tf tokenFile) bool {
	return strings.TrimSpace(tf.UID) != "" &&
		strings.TrimSpace(tf.AccessToken) != "" &&
		strings.TrimSpace(tf.RefreshToken) != ""
}

func readTokenFileFromPath(path string) (string, string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to read proton auth file")
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return "", "", "", fmt.Errorf("failed to parse proton auth file")
	}
	if !hasTokenFields(tf) {
		return "", "", "", fmt.Errorf("proton auth file missing uid/accessToken/refreshToken")
	}
	return strings.TrimSpace(tf.UID), strings.TrimSpace(tf.AccessToken), strings.TrimSpace(tf.RefreshToken), nil
}

func readTokenFile() (string, string, string, error) {
	path := tokenFilePath()
	uid, acc, ref, err := readTokenFileFromPath(path)
	if err == nil {
		return uid, acc, ref, nil
	}

	// Fallback to last-known-good snapshot when the live auth file is temporarily
	// malformed or mid-rotation.
	snapshotPath := tokenSnapshotFilePath(path)
	suid, sacc, sref, snapshotErr := readTokenFileFromPath(snapshotPath)
	if snapshotErr == nil {
		return suid, sacc, sref, nil
	}

	return "", "", "", err
}

func writeTokenFile(uid, acc, ref string) error {
	if strings.TrimSpace(uid) == "" || strings.TrimSpace(acc) == "" || strings.TrimSpace(ref) == "" {
		return nil
	}
	return updateTokenFile(func(existing map[string]any) {
		existing["uid"] = uid
		existing["accessToken"] = acc
		existing["refreshToken"] = ref
		existing["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	})
}

func updateTokenFile(mutate func(existing map[string]any)) error {
	path := tokenFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	// Preserve extra metadata fields (source, clientID, cookies, etc.).
	existing := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &existing)
	}

	mutate(existing)

	b, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}

	// Atomic replace while holding the inter-process lock.
	if err := atomicWritePrivateFile(path, b); err != nil {
		return err
	}

	// Keep a last-known-good snapshot when token fields are present.
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err == nil && hasTokenFields(tf) {
		if err := atomicWritePrivateFile(tokenSnapshotFilePath(path), b); err != nil {
			return err
		}
	}

	return nil
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

func (c *APIClient) ensureLabelID(ctx context.Context, name string) (string, error) {
	pc, err := c.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return "", errors.New("empty label")
	}

	c.mu.Lock()
	if id, ok := c.labelByKey[key]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	// Only resolve user labels here. Using folder IDs with LabelMessages can
	// report success without attaching a visible label in Proton Mail.
	labels, err := pc.GetLabels(ctx, protonapi.LabelTypeLabel)
	if err != nil {
		return "", err
	}
	for _, label := range labels {
		if strings.EqualFold(label.Name, name) {
			c.mu.Lock()
			c.labelByKey[key] = label.ID
			c.mu.Unlock()
			return label.ID, nil
		}
	}

	// If the same name exists as a folder, fail clearly instead of caching a
	// folder ID and silently no-op'ing label application.
	folders, err := pc.GetLabels(ctx, protonapi.LabelTypeFolder)
	if err != nil {
		return "", err
	}
	for _, folder := range folders {
		if strings.EqualFold(folder.Name, name) {
			return "", fmt.Errorf("%q exists as a folder in Proton; create a label with a distinct name", name)
		}
	}

	created, err := pc.CreateLabel(ctx, protonapi.CreateLabelReq{Name: name, Color: "#4A4A4A", Type: protonapi.LabelTypeLabel})
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.labelByKey[key] = created.ID
	c.mu.Unlock()
	return created.ID, nil
}

func containsLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

var tagPattern = regexp.MustCompile("<[^>]+>")

func isPGPArmored(s string) bool {
	return strings.Contains(s, "-----BEGIN PGP MESSAGE-----")
}

type messageDecrypter struct {
	keyRing *pgpcrypto.KeyRing
}

func (d *messageDecrypter) Decrypt(armored string) (string, error) {
	message, err := pgpcrypto.NewPGPMessageFromArmored(armored)
	if err != nil {
		return "", err
	}
	plain, err := d.keyRing.Decrypt(message, nil, 0)
	if err != nil {
		return "", err
	}
	return plain.GetString(), nil
}

func loadMessageDecrypter() (*messageDecrypter, bool, error) {
	keyPath := protonPrivateKeyPath()
	passwordPath := protonPrivateKeyPasswordPath()

	keyPayload, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read proton private key: %w", err)
	}
	passwordPayload, err := os.ReadFile(passwordPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read proton private key password: %w", err)
	}

	key, err := pgpcrypto.NewKeyFromArmored(string(keyPayload))
	if err != nil {
		return nil, true, fmt.Errorf("parse proton private key: %w", err)
	}
	locked, err := key.IsLocked()
	if err != nil {
		return nil, true, fmt.Errorf("inspect proton private key: %w", err)
	}
	if locked {
		password := []byte(strings.TrimSpace(string(passwordPayload)))
		if len(password) == 0 {
			return nil, true, errors.New("proton private key password is empty")
		}
		key, err = key.Unlock(password)
		if err != nil {
			return nil, true, fmt.Errorf("unlock proton private key: %w", err)
		}
	}

	keyRing, err := pgpcrypto.NewKeyRing(key)
	if err != nil {
		return nil, true, fmt.Errorf("build proton keyring: %w", err)
	}
	return &messageDecrypter{keyRing: keyRing}, true, nil
}

func protonPrivateKeyPath() string {
	if path := strings.TrimSpace(os.Getenv("PROTON_PRIVATE_KEY_FILE")); path != "" {
		return path
	}
	return filepath.Join(secretDirPath(), "proton-private-key.asc")
}

func protonPrivateKeyPasswordPath() string {
	if path := strings.TrimSpace(os.Getenv("PROTON_PRIVATE_KEY_PASSWORD_FILE")); path != "" {
		return path
	}
	return filepath.Join(secretDirPath(), "proton-private-key-password")
}

func secretDirPath() string {
	if path := strings.TrimSpace(os.Getenv("SECRET_DIR")); path != "" {
		return path
	}
	return "/llama_lab/private"
}

func htmlToText(input string) string {
	stripped := tagPattern.ReplaceAllString(input, " ")
	stripped = strings.ReplaceAll(stripped, "&nbsp;", " ")
	stripped = strings.ReplaceAll(stripped, "&amp;", "&")
	stripped = strings.ReplaceAll(stripped, "&lt;", "<")
	stripped = strings.ReplaceAll(stripped, "&gt;", ">")
	return strings.Join(strings.Fields(stripped), " ")
}

// StubClient is a temporary no-op implementation used during scaffolding.
type StubClient struct{}

func (s *StubClient) ListUnreadInbox(_ context.Context, _ string) ([]Message, string, error) {
	return []Message{}, "", nil
}

func (s *StubClient) ListLabels(_ context.Context) ([]string, error) {
	return []string{"Questionable", "Important", "Primary", "Updates", "Social", "Promotions"}, nil
}

func (s *StubClient) EnsureLabel(_ context.Context, _ string) error {
	return nil
}

func (s *StubClient) ApplyLabel(_ context.Context, _, _ string) error {
	return nil
}
