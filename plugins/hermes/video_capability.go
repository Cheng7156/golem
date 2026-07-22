package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/capability/video"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/mediaobject"
	"golem_plugin_hermes/internal/store/sqlite"
)

const maximumVideoCandidateRecords = 4096

type videoCapabilityBridge struct {
	service video.SearchService
	expires time.Duration
}

func newVideoCapability(
	value config.VideoCapabilityConfig,
	providerEnvironment func(string) (string, bool),
	repository *sqlite.Store,
) (agent.VideoCapability, *mediaobject.Store, error) {
	infrastructure, err := buildVideoInfrastructure(value, repository)
	if err != nil {
		return nil, nil, err
	}
	providers, err := buildVideoProviders(value, providerEnvironment)
	if err != nil {
		return nil, nil, err
	}
	service, err := video.NewSearchService(video.ServiceConfig{
		DefaultCategory: value.DefaultCategory,
		CandidateTTL:    time.Duration(value.CandidateTTLSeconds) * time.Second,
		MaxRecords:      maximumVideoCandidateRecords, MaxResults: value.MaxCandidates,
		MaxVideosPerRun: value.MaxVideosPerRun,
		PrepareTimeout:  time.Duration(value.PrepareTimeoutSeconds) * time.Second,
		PrepareWorkers:  value.PrepareWorkers,
	}, providers, infrastructure.pipeline)
	if err != nil {
		return nil, nil, err
	}
	return &videoCapabilityBridge{
		service: service, expires: time.Duration(value.CandidateTTLSeconds) * time.Second,
	}, infrastructure.objects, nil
}

type videoInfrastructure struct {
	objects  *mediaobject.Store
	pipeline *video.PreparationPipeline
}

func buildVideoInfrastructure(
	value config.VideoCapabilityConfig,
	repository *sqlite.Store,
) (videoInfrastructure, error) {
	objects, err := mediaobject.NewStore(mediaobject.Config{
		Directory: value.MediaDirectory, MaxStorageBytes: value.StorageMaxBytes,
		Retention: time.Duration(value.CacheTTLHours) * time.Hour,
	}, repository)
	if err != nil {
		return videoInfrastructure{}, fmt.Errorf("create video object store: %w", err)
	}
	workDirectory := filepath.Join(value.MediaDirectory, ".work")
	downloader, err := video.NewDownloader(video.DownloadConfig{
		Directory: filepath.Join(workDirectory, "downloads"), MaxBytes: value.MaxSourceBytes,
		Timeout: time.Duration(value.DownloadTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return videoInfrastructure{}, fmt.Errorf("create video downloader: %w", err)
	}
	processor, err := video.NewProcessor(video.ProcessorConfig{
		FFmpegPath: value.FFmpegPath, FFprobePath: value.FFprobePath,
		WorkingDirectory: filepath.Join(workDirectory, "processing"),
		MaxVideoBytes:    value.MaxVideoBytes, MaxDuration: uint32(value.MaxDurationSeconds),
	}, nil)
	if err != nil {
		return videoInfrastructure{}, fmt.Errorf("create video processor: %w", err)
	}
	pipeline, err := video.NewPreparationPipeline(
		video.PipelineConfig{MaxVideoBytes: value.MaxVideoBytes},
		video.PipelineDependencies{Downloader: downloader, Processor: processor, Objects: objects},
	)
	if err != nil {
		return videoInfrastructure{}, err
	}
	return videoInfrastructure{objects: objects, pipeline: pipeline}, nil
}

func buildVideoProviders(
	value config.VideoCapabilityConfig,
	providerEnvironment func(string) (string, bool),
) ([]video.RegisteredProvider, error) {
	result := make([]video.RegisteredProvider, 0, len(value.Providers))
	for _, providerConfig := range value.Providers {
		if providerConfig.Disabled {
			continue
		}
		provider, err := video.NewHTTPProvider(httpVideoConfig(value, providerConfig),
			video.WithEnvironment(providerEnvironment))
		if err != nil {
			return nil, fmt.Errorf("video provider %s: %w", providerConfig.ID, err)
		}
		result = append(result, video.RegisteredProvider{
			Provider: provider, Priority: providerConfig.Priority,
		})
	}
	return result, nil
}

func httpVideoConfig(
	capability config.VideoCapabilityConfig,
	provider config.VideoProviderConfig,
) video.HTTPProviderConfig {
	return video.HTTPProviderConfig{
		ProviderID: provider.ID, Categories: provider.Categories, Endpoint: provider.Endpoint,
		Method: provider.Method, RequestMode: provider.RequestMode,
		ResponseMode: provider.ResponseMode, MaterializationMode: provider.MaterializationMode,
		Headers: provider.Headers, MediaHeaders: provider.MediaHeaders,
		Query: provider.Query, Form: provider.Form, JSONBody: provider.JSONBody,
		AllowedMediaHosts: provider.AllowedMediaHosts,
		PublicFallbackURL: provider.PublicFallbackURL, FallbackURLPolicy: provider.FallbackURLPolicy,
		Response:          videoResponseMapping(provider.Response),
		Timeout:           time.Duration(provider.TimeoutSeconds) * time.Second,
		RequestsPerMinute: provider.RequestsPerMinute,
		MaxMetadataBytes:  provider.MaxResponseBytes, MaxSourceBytes: capability.MaxSourceBytes,
	}
}

func videoResponseMapping(value config.VideoResponseConfig) video.ResponseMapping {
	return video.ResponseMapping{
		SuccessPath: value.SuccessPath, SuccessValues: value.SuccessValues,
		ItemsPath: value.ItemsPath, ErrorPath: value.ErrorPath,
		URL: videoFieldMapping(value.URL), Title: videoFieldMapping(value.Title),
		PageURL: videoFieldMapping(value.PageURL), Duration: videoFieldMapping(value.Duration),
	}
}

func videoFieldMapping(value config.VideoFieldMapping) video.FieldMapping {
	return video.FieldMapping{Path: value.Path, Transforms: value.Transforms}
}

func (b *videoCapabilityBridge) Search(
	ctx context.Context,
	scope agent.VideoScope,
	input agent.VideoSearchInput,
) (agent.VideoSearchResult, error) {
	result, err := b.service.Search(ctx, video.SearchRequest{
		Scope:      video.Scope{RunID: scope.RunID, ChatID: scope.ChatID},
		ProviderID: input.ProviderID, Query: input.Query, Category: input.Category, Limit: input.Limit,
	})
	if err != nil {
		if errors.Is(err, video.ErrProviderNotFound) || errors.Is(err, video.ErrProviderCategory) {
			return agent.VideoSearchResult{}, fmt.Errorf("%w: %v", agent.ErrInvalidVideoSearch, err)
		}
		return agent.VideoSearchResult{}, err
	}
	return bridgeVideoSearchResult(result, b.expires), nil
}

func (b *videoCapabilityBridge) ResolveURL(
	ctx context.Context,
	scope agent.VideoScope,
	input agent.VideoURLInput,
) (agent.VideoCandidate, error) {
	candidate, err := b.service.RegisterURL(ctx, video.URLRequest{
		Scope: video.Scope{RunID: scope.RunID, ChatID: scope.ChatID}, URL: input.URL, Title: input.Title,
	})
	if err != nil {
		return agent.VideoCandidate{}, err
	}
	return bridgeVideoCandidate(candidate), nil
}

func (b *videoCapabilityBridge) Select(
	ctx context.Context,
	scope agent.VideoScope,
	candidateID string,
) (domain.VideoOutput, error) {
	return b.service.Select(ctx, video.Scope{RunID: scope.RunID, ChatID: scope.ChatID}, candidateID)
}

func (b *videoCapabilityBridge) Release(scope agent.VideoScope) {
	b.service.Release(video.Scope{RunID: scope.RunID, ChatID: scope.ChatID})
}

func bridgeVideoSearchResult(value video.SearchResult, expires time.Duration) agent.VideoSearchResult {
	result := agent.VideoSearchResult{ExpiresInSeconds: int(expires / time.Second)}
	for _, candidate := range value.Candidates {
		result.Candidates = append(result.Candidates, bridgeVideoCandidate(candidate))
	}
	for _, failure := range value.Failures {
		result.Failures = append(result.Failures, agent.VideoProviderFailure{
			ProviderID: failure.ProviderID, Message: failure.Message,
		})
	}
	return result
}

func bridgeVideoCandidate(value video.Candidate) agent.VideoCandidate {
	return agent.VideoCandidate{
		ID: value.ID, ProviderID: value.ProviderID, Title: value.Title,
		PageURL: value.PageURL, DurationSeconds: value.DurationSeconds,
	}
}
