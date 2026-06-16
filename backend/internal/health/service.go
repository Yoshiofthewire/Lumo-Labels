package health

import (
	"sync"
	"time"
)

type Status struct {
	Healthy              bool     `json:"healthy"`
	UnhealthyFor         int64    `json:"unhealthyForSeconds"`
	LastCheckUTC         string   `json:"lastCheckUtc"`
	FailureReason        []string `json:"failureReason"`
	AICreditsExhausted   bool     `json:"aiCreditsExhausted"`
	AICreditsExhaustedAt string   `json:"aiCreditsExhaustedAt,omitempty"`
}

type Service struct {
	mu             sync.Mutex
	status         Status
	unhealthySince *time.Time

	// AI-credits flag is sticky: it is preserved across SetStatus/MarkHealthy
	// calls and only changes via SetAICreditsExhausted / ClearAICreditsExhausted.
	aiCreditsExhausted   bool
	aiCreditsExhaustedAt string
}

func NewService() *Service {
	now := time.Now().UTC().Format(time.RFC3339)
	return &Service{status: Status{Healthy: true, LastCheckUTC: now}}
}

func (s *Service) SetStatus(st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.Healthy {
		s.unhealthySince = nil
		st.UnhealthyFor = 0
	} else if s.unhealthySince == nil {
		now := time.Now().UTC()
		s.unhealthySince = &now
	}
	st.LastCheckUTC = time.Now().UTC().Format(time.RFC3339)
	if s.unhealthySince != nil {
		st.UnhealthyFor = int64(time.Since(*s.unhealthySince).Seconds())
	}
	s.status = st
}

func (s *Service) MarkHealthy() {
	s.SetStatus(Status{Healthy: true, FailureReason: nil})
}

func (s *Service) MarkUnhealthy(reasons ...string) {
	s.SetStatus(Status{Healthy: false, FailureReason: reasons})
}

func (s *Service) GetStatus() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	if s.unhealthySince != nil {
		st.UnhealthyFor = int64(time.Since(*s.unhealthySince).Seconds())
	}
	st.LastCheckUTC = time.Now().UTC().Format(time.RFC3339)
	st.AICreditsExhausted = s.aiCreditsExhausted
	st.AICreditsExhaustedAt = s.aiCreditsExhaustedAt
	return st
}

// SetAICreditsExhausted raises the sticky AI-credits flag. It is independent of
// the healthy/unhealthy status so it survives MarkHealthy/MarkUnhealthy calls.
func (s *Service) SetAICreditsExhausted(atUTC string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aiCreditsExhausted = true
	s.aiCreditsExhaustedAt = atUTC
}

// ClearAICreditsExhausted lowers the sticky AI-credits flag.
func (s *Service) ClearAICreditsExhausted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aiCreditsExhausted = false
	s.aiCreditsExhaustedAt = ""
}
