package sticker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type ServiceConfig struct {
	DefaultProvider           string
	CandidateTTL              time.Duration
	MaxCandidates             int
	MaxResults                int
	MaterializedCacheMaxBytes int64
}

type candidateRecord struct {
	public       Candidate
	scope        Scope
	provider     string
	value        ProviderCandidate
	created      time.Time
	lastAccess   time.Time
	materialized *domain.EmojiOutput
	inflight     *materializeCall
}

type service struct {
	mu            sync.Mutex
	providers     map[string]Provider
	order         []string
	defaultID     string
	ttl           time.Duration
	maxRecords    int
	maxResults    int
	maxCacheBytes int64
	cacheBytes    int64
	now           func() time.Time
	records       map[string]*candidateRecord
}

type ServiceOption func(*service)

// WithServiceClock is primarily useful for deterministic expiry tests.
func WithServiceClock(clock func() time.Time) ServiceOption {
	return func(s *service) {
		if clock != nil {
			s.now = clock
		}
	}
}

func NewSearchService(config ServiceConfig, providers []Provider, options ...ServiceOption) (SearchService, error) {
	if config.CandidateTTL <= 0 {
		config.CandidateTTL = 5 * time.Minute
	}
	if config.MaxCandidates <= 0 {
		config.MaxCandidates = 4096
	}
	if config.MaxResults <= 0 {
		config.MaxResults = 20
	}
	if config.MaterializedCacheMaxBytes <= 0 {
		config.MaterializedCacheMaxBytes = 64 << 20
	}
	value := &service{
		providers:     make(map[string]Provider, len(providers)),
		defaultID:     strings.TrimSpace(config.DefaultProvider),
		ttl:           config.CandidateTTL,
		maxRecords:    config.MaxCandidates,
		maxResults:    config.MaxResults,
		maxCacheBytes: config.MaterializedCacheMaxBytes,
		now:           time.Now,
		records:       make(map[string]*candidateRecord),
	}
	for _, option := range options {
		option(value)
	}
	for _, provider := range providers {
		if provider == nil {
			return nil, errors.New("nil sticker provider")
		}
		id := strings.TrimSpace(provider.ID())
		if id == "" {
			return nil, errors.New("sticker provider id is empty")
		}
		if _, exists := value.providers[id]; exists {
			return nil, fmt.Errorf("duplicate sticker provider %q", id)
		}
		value.providers[id] = provider
		value.order = append(value.order, id)
	}
	if len(value.providers) == 0 {
		return nil, errors.New("at least one sticker provider is required")
	}
	if value.defaultID == "" && len(value.order) == 1 {
		value.defaultID = value.order[0]
	}
	if value.defaultID == "" {
		return nil, errors.New("default sticker provider is required when multiple providers are configured")
	}
	if _, exists := value.providers[value.defaultID]; !exists {
		return nil, fmt.Errorf("default sticker provider %q: %w", value.defaultID, ErrProviderNotFound)
	}
	return value, nil
}

func (s *service) Search(ctx context.Context, request SearchRequest) ([]Candidate, error) {
	if err := request.Scope.validate(); err != nil {
		return nil, err
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return nil, errors.New("sticker search query is empty")
	}
	if request.Limit <= 0 {
		request.Limit = min(10, s.maxResults)
	}
	request.Limit = min(request.Limit, s.maxResults)
	if request.Page <= 0 {
		request.Page = 1
	}
	providerID := strings.TrimSpace(request.ProviderID)
	if providerID == "" {
		providerID = s.defaultID
	}
	provider := s.providers[providerID]
	if provider == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	values, err := provider.Search(ctx, ProviderSearchRequest{
		Query: request.Query,
		Limit: request.Limit,
		Page:  request.Page,
	})
	if err != nil {
		return nil, err
	}
	if len(values) > request.Limit {
		values = values[:request.Limit]
	}
	return s.Bind(ctx, request.Scope, providerID, values)
}

func (s *service) Bind(
	ctx context.Context,
	scope Scope,
	providerID string,
	values []ProviderCandidate,
) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.validate(); err != nil {
		return nil, err
	}
	providerID = strings.TrimSpace(providerID)
	if s.providers[providerID] == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	if len(values) > 100 {
		return nil, errors.New("too many sticker candidates to bind")
	}
	now := s.now()
	expires := now.Add(s.ttl)
	result := make([]Candidate, 0, len(values))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	for _, item := range values {
		if strings.TrimSpace(item.Reference) == "" {
			continue
		}
		id, idErr := newCandidateID()
		if idErr != nil {
			return nil, idErr
		}
		candidate := Candidate{
			ID:          id,
			ProviderID:  providerID,
			Description: strings.TrimSpace(item.Description),
			ExpiresAt:   expires,
		}
		s.records[id] = &candidateRecord{
			public:     candidate,
			scope:      scope,
			provider:   providerID,
			value:      item,
			created:    now,
			lastAccess: now,
		}
		result = append(result, candidate)
	}
	s.enforceCapacityLocked()
	return result, nil
}

func (s *service) purgeExpiredLocked(now time.Time) {
	for id, record := range s.records {
		if !now.Before(record.public.ExpiresAt) {
			s.deleteRecordLocked(id)
		}
	}
}

func (s *service) enforceCapacityLocked() {
	for len(s.records) > s.maxRecords {
		var oldestID string
		var oldest time.Time
		for id, record := range s.records {
			if oldestID == "" || record.created.Before(oldest) {
				oldestID = id
				oldest = record.created
			}
		}
		s.deleteRecordLocked(oldestID)
	}
}

func (s *service) deleteRecordLocked(candidateID string) {
	record := s.records[candidateID]
	if record == nil {
		return
	}
	if record.materialized != nil {
		s.cacheBytes -= int64(len(record.materialized.Data))
	}
	delete(s.records, candidateID)
}

func newCandidateID() (string, error) {
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create sticker candidate id: %w", err)
	}
	return "stc_" + base64.RawURLEncoding.EncodeToString(random[:]), nil
}
