package video

import (
	"context"
	"fmt"
	"os"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/mediaobject"
)

const maximumThumbnailBytes int64 = 2 << 20

type SourceDownloader interface {
	Download(context.Context, MediaSource) (DownloadedMedia, error)
}

type SourceProcessor interface {
	Prepare(context.Context, DownloadedMedia) (PreparedMedia, error)
}

type ObjectWriter interface {
	Save(context.Context, mediaobject.SaveRequest) (domain.MediaObject, error)
}

type PreparationPipeline struct {
	downloader SourceDownloader
	processor  SourceProcessor
	objects    ObjectWriter
	maxVideo   int64
}

type PipelineConfig struct {
	MaxVideoBytes int64
}

type PipelineDependencies struct {
	Downloader SourceDownloader
	Processor  SourceProcessor
	Objects    ObjectWriter
}

func NewPreparationPipeline(
	config PipelineConfig,
	dependencies PipelineDependencies,
) (*PreparationPipeline, error) {
	if dependencies.Downloader == nil || dependencies.Processor == nil ||
		dependencies.Objects == nil || config.MaxVideoBytes <= 0 {
		return nil, fmt.Errorf("video preparation pipeline dependencies are invalid")
	}
	return &PreparationPipeline{
		downloader: dependencies.Downloader, processor: dependencies.Processor,
		objects: dependencies.Objects, maxVideo: config.MaxVideoBytes,
	}, nil
}

func (p *PreparationPipeline) Prepare(
	ctx context.Context,
	resolved ResolvedVideo,
) (domain.VideoOutput, error) {
	downloaded, err := p.downloader.Download(ctx, resolved.Source)
	if err != nil {
		return domain.VideoOutput{}, fmt.Errorf("download video: %w", err)
	}
	defer downloaded.Remove()
	prepared, err := p.processor.Prepare(ctx, downloaded)
	if err != nil {
		return domain.VideoOutput{}, fmt.Errorf("process video: %w", err)
	}
	defer prepared.Remove()
	videoObject, err := p.saveFile(ctx, preparedFileRequest{
		path: prepared.VideoPath, kind: domain.MediaKindVideo, mimeType: "video/mp4", maximum: p.maxVideo,
	})
	if err != nil {
		return domain.VideoOutput{}, err
	}
	thumbObject, err := p.saveFile(ctx, preparedFileRequest{
		path: prepared.ThumbnailPath, kind: domain.MediaKindThumbnail,
		mimeType: "image/jpeg", maximum: maximumThumbnailBytes,
	})
	if err != nil {
		return domain.VideoOutput{}, err
	}
	return domain.VideoOutput{
		ObjectID: videoObject.ID, ThumbObjectID: thumbObject.ID, Duration: prepared.Duration,
		Title: resolved.Title, PageURL: resolved.FallbackURL,
	}, nil
}

type preparedFileRequest struct {
	path     string
	kind     string
	mimeType string
	maximum  int64
}

func (p *PreparationPipeline) saveFile(
	ctx context.Context,
	request preparedFileRequest,
) (domain.MediaObject, error) {
	file, err := os.Open(request.path)
	if err != nil {
		return domain.MediaObject{}, fmt.Errorf("open prepared media: %w", err)
	}
	defer file.Close()
	return p.objects.Save(ctx, mediaobject.SaveRequest{
		Kind: request.kind, MIMEType: request.mimeType, Reader: file, MaxBytes: request.maximum,
	})
}
