package proton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	protonapi "github.com/ProtonMail/go-proton-api"
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

type APIClient struct {
	mu         sync.Mutex
	mgr        *protonapi.Manager
	client     *protonapi.Client
	labelByKey map[string]string
	host       string
	versions   []string
	versionIdx int
}

type tokenFile struct {
	UID          string `json:"uid"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

func NewAPIClientFromEnv() *APIClient {
	host := strings.TrimSpace(os.Getenv("PROTON_API_HOST"))
	versions := protonAppVersionsFromEnv()
	client := &APIClient{
		host:       host,
		versions:   versions,
		versionIdx: 0,
		labelByKey: map[string]string{},
	}
	client.mgr = newManager(host, client.currentVersion())
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

	pc.AddAuthHandler(func(a protonapi.Auth) {
		_ = writeTokenFile(a.UID, a.AccessToken, a.RefreshToken)
	})
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
	if err := c.refreshClient(ctx); err != nil {
		return nil, "", fmt.Errorf("fetch unread inbox failed: token refresh: %w", err)
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
		lastErr = err
		if !c.rotateVersionOnOutOfDate(err) {
			break
		}
	}

	return nil, "", lastErr
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
		// Body contains PGP-armored ciphertext; skip it rather than sending
		// the raw blob to Lumo. Classification uses Subject + Sender instead.
		if isPGPArmored(full.Body) {
			msg.Body = ""
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

func newManager(host, appVersion string) *protonapi.Manager {
	opts := []protonapi.Option{}
	if strings.TrimSpace(appVersion) != "" {
		opts = append(opts, protonapi.WithAppVersion(strings.TrimSpace(appVersion)))
	}
	if strings.TrimSpace(host) != "" {
		opts = append(opts, protonapi.WithHostURL(strings.TrimSpace(host)))
	}
	return protonapi.New(opts...)
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
	c.mgr = newManager(c.host, c.versions[c.versionIdx])
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
	return "/lumo_lab/config/proton-auth.json"
}

func readTokenFile() (string, string, string, error) {
	b, err := os.ReadFile(tokenFilePath())
	if err != nil {
		return "", "", "", errors.New("failed to read proton auth file")
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return "", "", "", errors.New("failed to parse proton auth file")
	}
	uid := strings.TrimSpace(tf.UID)
	acc := strings.TrimSpace(tf.AccessToken)
	ref := strings.TrimSpace(tf.RefreshToken)
	if uid == "" || acc == "" || ref == "" {
		return "", "", "", errors.New("proton auth file missing uid/accessToken/refreshToken")
	}
	return uid, acc, ref, nil
}

func writeTokenFile(uid, acc, ref string) error {
	b, err := json.Marshal(tokenFile{UID: uid, AccessToken: acc, RefreshToken: ref})
	if err != nil {
		return err
	}
	return os.WriteFile(tokenFilePath(), b, 0600)
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

	labels, err := pc.GetLabels(ctx, protonapi.LabelTypeLabel, protonapi.LabelTypeFolder)
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
