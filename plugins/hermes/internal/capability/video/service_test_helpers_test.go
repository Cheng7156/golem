package video

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/mediaobject"
)

type serviceTestProvider struct {
	id            string
	categories    []string
	discoverError error
	resolveError  error
	fallbackURL   string
	resolveCalls  atomic.Int32
}

func (p *serviceTestProvider) ID() string { return p.id }

func (p *serviceTestProvider) Categories() []string {
	return append([]string(nil), p.categories...)
}

func (p *serviceTestProvider) Discover(
	context.Context,
	DiscoveryRequest,
) ([]ProviderCandidate, error) {
	if p.discoverError != nil {
		return nil, p.discoverError
	}
	return []ProviderCandidate{{Reference: "provider-reference", Title: p.id + " video"}}, nil
}

func (p *serviceTestProvider) Resolve(
	context.Context,
	ProviderCandidate,
) (ResolvedVideo, error) {
	p.resolveCalls.Add(1)
	if p.resolveError != nil {
		return ResolvedVideo{}, p.resolveError
	}
	return ResolvedVideo{
		Title: p.id + " video", FallbackURL: p.fallbackURL,
		Source: MediaSource{Body: io.NopCloser(bytes.NewReader(
			append([]byte{0, 0, 0, 24}, []byte("ftypisom-video")...),
		))},
	}, nil
}

func (p *serviceTestProvider) FallbackURL(ProviderCandidate) string {
	return p.fallbackURL
}

type serviceTestProcessor struct {
	directory string
	err       error
}

func (p serviceTestProcessor) Prepare(
	_ context.Context,
	input DownloadedMedia,
) (PreparedMedia, error) {
	if p.err != nil {
		return PreparedMedia{}, p.err
	}
	thumbnail := filepath.Join(p.directory, "thumb.jpg")
	if err := os.WriteFile(thumbnail, []byte("thumbnail"), 0o600); err != nil {
		return PreparedMedia{}, err
	}
	return PreparedMedia{VideoPath: input.Path, ThumbnailPath: thumbnail, Duration: 2}, nil
}

type serviceTestObjects struct{}

func (serviceTestObjects) Save(
	_ context.Context,
	request mediaobject.SaveRequest,
) (domain.MediaObject, error) {
	if _, err := io.ReadAll(request.Reader); err != nil {
		return domain.MediaObject{}, err
	}
	return domain.MediaObject{ID: request.Kind + "-object"}, nil
}

func newServiceTestPipeline(t *testing.T, processor SourceProcessor) *PreparationPipeline {
	t.Helper()
	downloader, err := NewDownloader(DownloadConfig{
		Directory: filepath.Join(t.TempDir(), "downloads"), MaxBytes: 1024, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewDownloader: %v", err)
	}
	pipeline, err := NewPreparationPipeline(PipelineConfig{MaxVideoBytes: 1024}, PipelineDependencies{
		Downloader: downloader, Processor: processor, Objects: serviceTestObjects{},
	})
	if err != nil {
		t.Fatalf("NewPreparationPipeline: %v", err)
	}
	return pipeline
}

type serviceTestConfig struct {
	providers []RegisteredProvider
	processor SourceProcessor
	maximum   int
}

func newServiceForTest(t *testing.T, config serviceTestConfig) SearchService {
	t.Helper()
	service, err := NewSearchService(ServiceConfig{
		CandidateTTL: time.Minute, MaxRecords: 20, MaxResults: 5,
		MaxVideosPerRun: config.maximum, PrepareTimeout: time.Second, PrepareWorkers: 1,
	}, config.providers, newServiceTestPipeline(t, config.processor))
	if err != nil {
		t.Fatalf("NewSearchService: %v", err)
	}
	return service
}

var errTestPreparation = errors.New("test preparation failed")
