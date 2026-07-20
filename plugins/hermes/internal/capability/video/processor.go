package video

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type Processor struct {
	config ProcessorConfig
	runner CommandRunner
}

func NewProcessor(config ProcessorConfig, runner CommandRunner) (*Processor, error) {
	config.FFmpegPath = strings.TrimSpace(config.FFmpegPath)
	config.FFprobePath = strings.TrimSpace(config.FFprobePath)
	config.WorkingDirectory = filepath.Clean(strings.TrimSpace(config.WorkingDirectory))
	if config.FFmpegPath == "" || config.FFprobePath == "" || config.WorkingDirectory == "." {
		return nil, errors.New("video processor paths are invalid")
	}
	if config.MaxVideoBytes <= 0 || config.MaxDuration == 0 {
		return nil, errors.New("video processor limits are invalid")
	}
	if runner == nil {
		if err := validateExecutable(config.FFmpegPath); err != nil {
			return nil, err
		}
		if err := validateExecutable(config.FFprobePath); err != nil {
			return nil, err
		}
		runner = execCommandRunner{}
		if err := validateProcessorTools(config, runner); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(config.WorkingDirectory, 0o750); err != nil {
		return nil, fmt.Errorf("create video processor directory: %w", err)
	}
	return &Processor{config: config, runner: runner}, nil
}

func (p *Processor) Prepare(ctx context.Context, input DownloadedMedia) (PreparedMedia, error) {
	info, err := p.probe(ctx, input.Path)
	if err != nil {
		return PreparedMedia{}, err
	}
	if err := p.validateInfo(info); err != nil {
		return PreparedMedia{}, err
	}
	videoPath := input.Path
	ownedVideo := false
	transcode, err := p.requiresTranscode(input.Path, info)
	if err != nil {
		return PreparedMedia{}, err
	}
	if transcode {
		videoPath, err = p.transcode(ctx, input.Path, info.Duration)
		if err != nil {
			return PreparedMedia{}, err
		}
		ownedVideo = true
	}
	prepared, err := p.finishPrepared(ctx, videoPath)
	if err != nil && ownedVideo {
		_ = os.Remove(videoPath)
	}
	return prepared, err
}

func (p *Processor) requiresTranscode(path string, info MediaInfo) (bool, error) {
	if !compatibleMedia(info) {
		return true, nil
	}
	file, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat source video: %w", err)
	}
	return file.Size() > p.config.MaxVideoBytes, nil
}

func (p *Processor) finishPrepared(
	ctx context.Context,
	videoPath string,
) (PreparedMedia, error) {
	info, err := p.probe(ctx, videoPath)
	if err != nil {
		return PreparedMedia{}, err
	}
	if err := p.validatePreparedFile(videoPath, info); err != nil {
		return PreparedMedia{}, err
	}
	thumbnail, err := p.thumbnail(ctx, videoPath, info.Duration)
	if err != nil {
		return PreparedMedia{}, err
	}
	result := PreparedMedia{
		VideoPath: videoPath, ThumbnailPath: thumbnail,
		Duration: uint32(math.Ceil(info.Duration)),
	}
	return result, nil
}

func (p *Processor) validateInfo(info MediaInfo) error {
	if info.Duration <= 0 {
		return errors.New("ffprobe did not report a positive video duration")
	}
	if info.Duration > float64(p.config.MaxDuration) {
		return fmt.Errorf("video duration %.3fs exceeds %ds", info.Duration, p.config.MaxDuration)
	}
	if info.VideoCodec == "" || info.Width <= 0 || info.Height <= 0 {
		return errors.New("ffprobe did not report a usable video stream")
	}
	return nil
}

func (p *Processor) validatePreparedFile(path string, info MediaInfo) error {
	if err := p.validateInfo(info); err != nil {
		return err
	}
	if !compatibleMedia(info) {
		return errors.New("prepared video is not MP4/H.264 with supported audio")
	}
	file, err := os.Stat(path)
	if err != nil {
		return err
	}
	if file.Size() <= 0 || file.Size() > p.config.MaxVideoBytes {
		return fmt.Errorf("prepared video size %d exceeds limit %d", file.Size(), p.config.MaxVideoBytes)
	}
	return nil
}

func compatibleMedia(info MediaInfo) bool {
	format := strings.ToLower(info.Format)
	containerOK := strings.Contains(format, "mp4") || strings.Contains(format, "mov")
	audioOK := info.AudioCodec == "" || strings.EqualFold(info.AudioCodec, "aac")
	return containerOK && strings.EqualFold(info.VideoCodec, "h264") && audioOK &&
		info.Width <= maximumOutputWidth && info.Height <= maximumOutputHeight
}
