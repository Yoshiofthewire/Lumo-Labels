package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu           sync.Mutex
	baseDir      string
	checkpoint   string
	processedSet map[string]time.Time
	decisions    []Decision

	aiCreditsExhausted   bool
	aiCreditsExhaustedAt string
}

type Decision struct {
	MessageID string `json:"messageId"`
	Sender    string `json:"sender"`
	Subject   string `json:"subject"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	AtUTC     string `json:"atUtc"`
}

type stateFile struct {
	LastCheckpoint       string            `json:"lastCheckpoint"`
	Processed            map[string]string `json:"processed"`
	AICreditsExhausted   bool              `json:"aiCreditsExhausted,omitempty"`
	AICreditsExhaustedAt string            `json:"aiCreditsExhaustedAt,omitempty"`
}

func New(baseDir string) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{baseDir: baseDir, processedSet: map[string]time.Time{}, decisions: []Decision{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := s.loadDecisions(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.baseDir, "state.json")
}

func (s *Store) decisionsPath() string {
	return filepath.Join(s.baseDir, "decisions.json")
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.persistLocked()
		}
		return err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return err
	}
	s.checkpoint = sf.LastCheckpoint
	s.aiCreditsExhausted = sf.AICreditsExhausted
	s.aiCreditsExhaustedAt = sf.AICreditsExhaustedAt
	for id, ts := range sf.Processed {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		s.processedSet[id] = t
	}
	return nil
}

func (s *Store) Cleanup(keepDays int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)
	for id, ts := range s.processedSet {
		if ts.Before(cutoff) {
			delete(s.processedSet, id)
		}
	}
	trimmed := make([]Decision, 0, len(s.decisions))
	for _, d := range s.decisions {
		t, err := time.Parse(time.RFC3339, d.AtUTC)
		if err != nil || !t.Before(cutoff) {
			trimmed = append(trimmed, d)
		}
	}
	s.decisions = trimmed
	if err := s.persistDecisionsLocked(); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.processedSet[id]
	return ok
}

func (s *Store) MarkProcessed(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processedSet[id] = time.Now().UTC()
	return s.persistLocked()
}

func (s *Store) SetCheckpoint(value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint = value
	return s.persistLocked()
}

func (s *Store) Checkpoint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint
}

func (s *Store) AddDecision(d Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.AtUTC == "" {
		d.AtUTC = time.Now().UTC().Format(time.RFC3339)
	}
	s.decisions = append(s.decisions, d)
	return s.persistDecisionsLocked()
}

func (s *Store) Decisions(limit int) []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.decisions) == 0 {
		return []Decision{}
	}
	out := make([]Decision, len(s.decisions))
	copy(out, s.decisions)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AtUTC > out[j].AtUTC
	})
	if limit > 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}

func (s *Store) persistLocked() error {
	processed := make(map[string]string, len(s.processedSet))
	for id, ts := range s.processedSet {
		processed[id] = ts.Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(stateFile{
		LastCheckpoint:       s.checkpoint,
		Processed:            processed,
		AICreditsExhausted:   s.aiCreditsExhausted,
		AICreditsExhaustedAt: s.aiCreditsExhaustedAt,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path(), b, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// SetAICreditsExhausted marks that Llama reported the weekly chat limit / out of
// AI credits. It returns true only on the false->true transition so callers can
// notify exactly once until the flag is reset. The flag is persisted so it
// survives a restart.
func (s *Store) SetAICreditsExhausted(atUTC string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aiCreditsExhausted {
		return false, nil
	}
	if strings.TrimSpace(atUTC) == "" {
		atUTC = time.Now().UTC().Format(time.RFC3339)
	}
	s.aiCreditsExhausted = true
	s.aiCreditsExhaustedAt = atUTC
	return true, s.persistLocked()
}

// ClearAICreditsExhausted resets the AI-credits flag. It returns true only when
// the flag was previously set (true->false transition).
func (s *Store) ClearAICreditsExhausted() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aiCreditsExhausted {
		return false, nil
	}
	s.aiCreditsExhausted = false
	s.aiCreditsExhaustedAt = ""
	return true, s.persistLocked()
}

// AICreditsExhausted reports whether the AI-credits flag is set and when it was
// first raised.
func (s *Store) AICreditsExhausted() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aiCreditsExhausted, s.aiCreditsExhaustedAt
}

func (s *Store) loadDecisions() error {
	b, err := os.ReadFile(s.decisionsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.persistDecisionsLocked()
		}
		return err
	}
	var d []Decision
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	s.decisions = d
	return nil
}

func (s *Store) persistDecisionsLocked() error {
	b, err := json.MarshalIndent(s.decisions, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.decisionsPath(), b, 0o600); err != nil {
		return fmt.Errorf("write decisions: %w", err)
	}
	return nil
}
