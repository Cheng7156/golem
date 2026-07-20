package video

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type prepareCall struct {
	done   chan struct{}
	output domain.VideoOutput
	err    error
}

type candidateRecord struct {
	public   Candidate
	scope    Scope
	provider Provider
	value    ProviderCandidate
	created  time.Time
	prepared *domain.VideoOutput
	inflight *prepareCall
}

type service struct {
	mu              sync.Mutex
	providers       []RegisteredProvider
	providerByID    map[string]Provider
	defaultCategory string
	ttl             time.Duration
	maxRecords      int
	maxResults      int
	maxPerRun       int
	prepareTimeout  time.Duration
	prepareSlots    chan struct{}
	pipeline        *PreparationPipeline
	direct          Provider
	now             func() time.Time
	records         map[string]*candidateRecord
	runCounts       map[string]int
}

func NewSearchService(
	config ServiceConfig,
	providers []RegisteredProvider,
	pipeline *PreparationPipeline,
) (SearchService, error) {
	if pipeline == nil {
		return nil, errors.New("video preparation pipeline is required")
	}
	normalizeServiceConfig(&config)
	value := &service{
		providers:       append([]RegisteredProvider(nil), providers...),
		providerByID:    make(map[string]Provider, len(providers)),
		defaultCategory: config.DefaultCategory, ttl: config.CandidateTTL,
		maxRecords: config.MaxRecords, maxResults: config.MaxResults,
		maxPerRun: config.MaxVideosPerRun, prepareTimeout: config.PrepareTimeout,
		prepareSlots: make(chan struct{}, config.PrepareWorkers),
		pipeline:     pipeline, direct: directURLProvider{}, now: time.Now,
		records:   make(map[string]*candidateRecord),
		runCounts: make(map[string]int),
	}
	if err := value.registerProviders(); err != nil {
		return nil, err
	}
	return value, nil
}

func normalizeServiceConfig(config *ServiceConfig) {
	config.DefaultCategory = strings.ToLower(strings.TrimSpace(config.DefaultCategory))
	if config.DefaultCategory == "" {
		config.DefaultCategory = "general"
	}
	if config.CandidateTTL <= 0 {
		config.CandidateTTL = 10 * time.Minute
	}
	if config.MaxRecords <= 0 {
		config.MaxRecords = 4096
	}
	if config.MaxResults <= 0 {
		config.MaxResults = 5
	}
	if config.MaxVideosPerRun <= 0 {
		config.MaxVideosPerRun = 3
	}
	if config.PrepareTimeout <= 0 {
		config.PrepareTimeout = 3 * time.Minute
	}
	if config.PrepareWorkers <= 0 {
		config.PrepareWorkers = 2
	}
}

func (s *service) registerProviders() error {
	for _, registered := range s.providers {
		if registered.Provider == nil || strings.TrimSpace(registered.Provider.ID()) == "" {
			return errors.New("video provider is nil or has an empty id")
		}
		id := registered.Provider.ID()
		if _, exists := s.providerByID[id]; exists {
			return fmt.Errorf("duplicate video provider %q", id)
		}
		s.providerByID[id] = registered.Provider
	}
	sort.SliceStable(s.providers, func(left, right int) bool {
		return s.providers[left].Priority > s.providers[right].Priority
	})
	return nil
}
