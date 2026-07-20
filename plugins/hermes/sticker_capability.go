package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/capability/sticker"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

type stickerCapabilityBridge struct {
	service       sticker.SearchService
	expires       time.Duration
	maxCandidates int
	maxQueryRunes int
}

func newStickerCapability(
	value config.StickerCapabilityConfig,
	providerEnvironment func(string) (string, bool),
) (agent.StickerCapability, error) {
	providers := make([]sticker.Provider, 0, len(value.Providers))
	maxQueryRunes := 0
	for _, providerConfig := range value.Providers {
		if providerConfig.Disabled {
			continue
		}
		provider, err := sticker.NewHTTPJSONProvider(sticker.HTTPJSONConfig{
			ProviderID:            providerConfig.ID,
			Endpoint:              providerConfig.Endpoint,
			Method:                providerConfig.Method,
			Query:                 providerConfig.Query,
			Form:                  providerConfig.Form,
			Headers:               providerConfig.Headers,
			Timeout:               time.Duration(providerConfig.TimeoutSeconds) * time.Second,
			RequestsPerMinute:     providerConfig.RequestsPerMinute,
			MaxQueryRunes:         providerConfig.MaxQueryRunes,
			SuccessPath:           providerConfig.Response.SuccessPath,
			SuccessValues:         providerConfig.Response.SuccessValues,
			ItemsPath:             providerConfig.Response.ItemsPath,
			URLPath:               providerConfig.Response.URL.Path,
			DescriptionPath:       providerConfig.Response.Description.Path,
			ErrorPath:             providerConfig.Response.ErrorPath,
			URLTransforms:         providerConfig.Response.URL.Transforms,
			DescriptionTransforms: providerConfig.Response.Description.Transforms,
			MediaAllowedHosts:     providerConfig.AllowedMediaHosts,
			MaxMediaBytes:         int64(value.MaxMediaBytes),
		}, sticker.WithProviderEnvironment(
			providerEnvironment,
		))
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", providerConfig.ID, err)
		}
		providers = append(providers, provider)
		if providerConfig.ID == value.DefaultProvider {
			maxQueryRunes = providerConfig.MaxQueryRunes
		}
	}
	service, err := sticker.NewSearchService(sticker.ServiceConfig{
		DefaultProvider:           value.DefaultProvider,
		CandidateTTL:              time.Duration(value.CandidateTTLSeconds) * time.Second,
		MaxCandidates:             4096,
		MaxResults:                value.MaxCandidates,
		MaterializedCacheMaxBytes: value.MaterializedCacheMaxBytes,
	}, providers)
	if err != nil {
		return nil, err
	}
	return &stickerCapabilityBridge{
		service:       service,
		expires:       time.Duration(value.CandidateTTLSeconds) * time.Second,
		maxCandidates: value.MaxCandidates,
		maxQueryRunes: maxQueryRunes,
	}, nil
}

func (b *stickerCapabilityBridge) Search(
	ctx context.Context,
	scope agent.StickerScope,
	query string,
	limit int,
) (agent.StickerSearchResult, error) {
	if b.maxQueryRunes > 0 {
		runes := []rune(query)
		if len(runes) > b.maxQueryRunes {
			query = string(runes[:b.maxQueryRunes])
		}
	}
	if limit <= 0 || limit > b.maxCandidates {
		limit = b.maxCandidates
	}
	values, err := b.service.Search(ctx, sticker.SearchRequest{
		Scope: sticker.Scope{RunID: scope.RunID, ChatID: scope.ChatID},
		Query: query,
		Limit: limit,
		Page:  1,
	})
	if err != nil {
		return agent.StickerSearchResult{}, err
	}
	result := agent.StickerSearchResult{
		Candidates:       make([]agent.StickerCandidate, 0, len(values)),
		ExpiresInSeconds: int(b.expires / time.Second),
	}
	for _, candidate := range values {
		result.Candidates = append(result.Candidates, agent.StickerCandidate{
			ID:          candidate.ID,
			Description: candidate.Description,
		})
	}
	return result, nil
}

func (b *stickerCapabilityBridge) Select(
	ctx context.Context,
	scope agent.StickerScope,
	candidateID string,
) (domain.EmojiOutput, error) {
	return b.Materialize(ctx, scope, candidateID)
}

func (b *stickerCapabilityBridge) Materialize(
	ctx context.Context,
	scope agent.StickerScope,
	candidateID string,
) (domain.EmojiOutput, error) {
	output, err := b.service.Materialize(
		ctx,
		sticker.Scope{RunID: scope.RunID, ChatID: scope.ChatID},
		candidateID,
	)
	if err != nil {
		return domain.EmojiOutput{}, mapStickerMaterializeError(err)
	}
	return output, nil
}

func mapStickerMaterializeError(err error) error {
	switch {
	case errors.Is(err, sticker.ErrCandidateNotFound),
		errors.Is(err, sticker.ErrCandidateExpired),
		errors.Is(err, sticker.ErrCandidateScope):
		return fmt.Errorf("%w: %v", agent.ErrStickerCandidateUnavailable, err)
	case errors.Is(err, sticker.ErrMaterializedCacheFull):
		return fmt.Errorf("%w: %v", agent.ErrStickerMaterializeCacheFull, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", agent.ErrStickerProviderTimeout, err)
	default:
		return fmt.Errorf("%w: %v", agent.ErrStickerProviderUnavailable, err)
	}
}
