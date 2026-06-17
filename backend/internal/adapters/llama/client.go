package llama

import (
	"context"
	"strings"
)

type Client interface {
	Classify(ctx context.Context, allowedLabels []string, sender, subject, body string) (string, error)
}

// StubClient is a temporary no-op implementation used during scaffolding.
type StubClient struct{}

func (s *StubClient) Classify(_ context.Context, allowedLabels []string, _, _, _ string) (string, error) {
	if len(allowedLabels) == 0 {
		return "", nil
	}
	return allowedLabels[0], nil
}

func SelectLabelFromText(allowedLabels []string, output string) string {
	if len(allowedLabels) == 0 {
		return ""
	}
	lowerOut := strings.ToLower(output)
	for _, label := range allowedLabels {
		if strings.EqualFold(label, "Questionable") && strings.Contains(lowerOut, "questionable") {
			return label
		}
	}
	for _, label := range allowedLabels {
		if strings.Contains(lowerOut, strings.ToLower(label)) {
			return label
		}
	}
	if strings.Contains(lowerOut, "important") {
		for _, label := range allowedLabels {
			if strings.EqualFold(label, "Important") {
				return label
			}
		}
	}
	return ""
}
