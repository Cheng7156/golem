package video

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (s *service) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	if err := request.Scope.validate(); err != nil {
		return SearchResult{}, err
	}
	request = s.normalizeSearchRequest(request)
	providers, err := s.matchProviders(request)
	if err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{}
	var failures []error
	for _, provider := range providers {
		remaining := request.Limit - len(result.Candidates)
		if remaining <= 0 {
			break
		}
		values, discoverErr := provider.Discover(ctx, DiscoveryRequest{
			Query: request.Query, Category: request.Category, Limit: remaining, Page: 1,
		})
		if discoverErr != nil {
			failures = append(failures, discoverErr)
			result.Failures = append(result.Failures, ProviderFailure{
				ProviderID: provider.ID(), Message: discoverErr.Error(),
			})
			continue
		}
		candidates, addErr := s.addCandidates(candidateBatch{
			scope: request.Scope, provider: provider, values: values, limit: remaining,
		})
		if addErr != nil {
			return SearchResult{}, addErr
		}
		result.Candidates = append(result.Candidates, candidates...)
	}
	if len(result.Candidates) > 0 {
		return result, nil
	}
	if len(failures) > 0 {
		return result, fmt.Errorf("all video providers failed: %w", errors.Join(failures...))
	}
	return result, ErrNoCandidates
}

func (s *service) normalizeSearchRequest(request SearchRequest) SearchRequest {
	request.ProviderID = strings.TrimSpace(request.ProviderID)
	request.Query = strings.TrimSpace(request.Query)
	request.Category = strings.ToLower(strings.TrimSpace(request.Category))
	if request.Category == "" {
		request.Category = s.defaultCategory
	}
	if request.Limit <= 0 || request.Limit > s.maxResults {
		request.Limit = s.maxResults
	}
	return request
}

func (s *service) matchProviders(request SearchRequest) ([]Provider, error) {
	if request.ProviderID != "" {
		provider := s.providerByID[request.ProviderID]
		if provider == nil {
			return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, request.ProviderID)
		}
		if !providerHandles(provider, request.Category) {
			return nil, fmt.Errorf("video provider %s does not handle category %s", provider.ID(), request.Category)
		}
		return []Provider{provider}, nil
	}
	result := make([]Provider, 0, len(s.providers))
	for _, registered := range s.providers {
		if providerHandles(registered.Provider, request.Category) {
			result = append(result, registered.Provider)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w for category %s", ErrProviderNotFound, request.Category)
	}
	return result, nil
}

func providerHandles(provider Provider, category string) bool {
	for _, value := range provider.Categories() {
		if strings.EqualFold(strings.TrimSpace(value), category) || strings.TrimSpace(value) == "*" {
			return true
		}
	}
	return false
}

type candidateBatch struct {
	scope    Scope
	provider Provider
	values   []ProviderCandidate
	limit    int
}

func (s *service) addCandidates(batch candidateBatch) ([]Candidate, error) {
	now := s.now()
	expires := now.Add(s.ttl)
	result := make([]Candidate, 0, min(len(batch.values), batch.limit))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	for _, value := range batch.values {
		if len(result) >= batch.limit || strings.TrimSpace(value.Reference) == "" {
			break
		}
		id, err := newVideoCandidateID()
		if err != nil {
			return nil, err
		}
		candidate := Candidate{
			ID: id, ProviderID: batch.provider.ID(), Title: strings.TrimSpace(value.Title),
			PageURL: value.PageURL, DurationSeconds: value.DurationSeconds, ExpiresAt: expires,
		}
		s.records[id] = &candidateRecord{
			public: candidate, scope: batch.scope, provider: batch.provider, value: value, created: now,
		}
		result = append(result, candidate)
	}
	s.enforceCapacityLocked()
	return result, nil
}
