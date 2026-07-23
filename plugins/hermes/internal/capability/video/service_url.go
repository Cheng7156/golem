package video

import (
	"context"
	"errors"
	"strings"
)

func (s *service) RegisterURL(ctx context.Context, request URLRequest) (Candidate, error) {
	if err := request.Scope.validate(); err != nil {
		return Candidate{}, err
	}
	if err := ctx.Err(); err != nil {
		return Candidate{}, err
	}
	value, err := newDirectURLCandidate(request.URL, request.Title, s.allowHTTP)
	if err != nil {
		return Candidate{}, err
	}
	candidates, err := s.addCandidates(candidateBatch{
		scope: request.Scope, provider: s.direct, values: []ProviderCandidate{value}, limit: 1,
	})
	if err != nil {
		return Candidate{}, err
	}
	if len(candidates) != 1 {
		return Candidate{}, errors.New("could not register video URL candidate")
	}
	return candidates[0], nil
}

func (s *service) Release(scope Scope) {
	runID := strings.TrimSpace(scope.RunID)
	chatID := strings.TrimSpace(scope.ChatID)
	if runID == "" || chatID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, record := range s.records {
		if record.scope.RunID == runID && record.scope.ChatID == chatID && record.inflight == nil {
			delete(s.records, id)
		}
	}
	delete(s.runCounts, runID)
}
