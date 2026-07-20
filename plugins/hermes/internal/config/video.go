package config

import (
	"errors"
	"path/filepath"
)

const (
	maximumVideoBytes        = 25 << 20
	maximumVideoSourceBytes  = 256 << 20
	maximumVideoCandidates   = 20
	maximumCandidateTTL      = 3600
	maximumProviderTimeout   = 120
	maximumRequestsPerMinute = 600
	maximumMetadataBytes     = 8 << 20
)

type videoProviderContext struct {
	index           int
	seen            map[string]struct{}
	defaultCategory string
}

func normalizeVideoCapability(video *VideoCapabilityConfig, defaults VideoCapabilityConfig) error {
	applyVideoDefaults(video, defaults)
	if err := validateVideoLimits(video); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(video.Providers))
	for index := range video.Providers {
		context := videoProviderContext{index: index, seen: seen, defaultCategory: video.DefaultCategory}
		if err := normalizeVideoProvider(&video.Providers[index], context); err != nil {
			return err
		}
	}
	return nil
}

func applyVideoDefaults(video *VideoCapabilityConfig, defaults VideoCapabilityConfig) {
	video.DefaultCategory = normalizeCategory(video.DefaultCategory)
	if video.DefaultCategory == "" {
		video.DefaultCategory = defaults.DefaultCategory
	}
	defaultPositiveInt(&video.MaxCandidates, defaults.MaxCandidates)
	defaultPositiveInt(&video.CandidateTTLSeconds, defaults.CandidateTTLSeconds)
	defaultPositiveInt64(&video.MaxSourceBytes, defaults.MaxSourceBytes)
	defaultPositiveInt64(&video.MaxVideoBytes, defaults.MaxVideoBytes)
	defaultPositiveInt(&video.MaxDurationSeconds, defaults.MaxDurationSeconds)
	defaultPositiveInt(&video.MaxVideosPerRun, defaults.MaxVideosPerRun)
	defaultPositiveInt(&video.PrepareTimeoutSeconds, defaults.PrepareTimeoutSeconds)
	defaultPositiveInt(&video.DownloadTimeoutSeconds, defaults.DownloadTimeoutSeconds)
	defaultPositiveInt(&video.PrepareWorkers, defaults.PrepareWorkers)
	defaultPositiveInt64(&video.StorageMaxBytes, defaults.StorageMaxBytes)
	defaultPositiveInt(&video.CacheTTLHours, defaults.CacheTTLHours)
	video.FFmpegPath = defaultVideoPath(video.FFmpegPath, defaults.FFmpegPath)
	video.FFprobePath = defaultVideoPath(video.FFprobePath, defaults.FFprobePath)
	video.MediaDirectory = defaultVideoPath(video.MediaDirectory, defaults.MediaDirectory)
}

func validateVideoLimits(video *VideoCapabilityConfig) error {
	if video.MaxCandidates > maximumVideoCandidates {
		return errors.New("capabilities.video.max_candidates 不能大于 20")
	}
	if video.CandidateTTLSeconds > maximumCandidateTTL {
		return errors.New("capabilities.video.candidate_ttl_seconds 不能大于 3600")
	}
	if video.MaxVideoBytes > maximumVideoBytes {
		return errors.New("capabilities.video.max_video_bytes 不能大于 25 MiB")
	}
	if video.MaxSourceBytes > maximumVideoSourceBytes {
		return errors.New("capabilities.video.max_source_bytes 不能大于 256 MiB")
	}
	if video.MaxSourceBytes < video.MaxVideoBytes {
		return errors.New("capabilities.video.max_source_bytes 不能小于 max_video_bytes")
	}
	if video.StorageMaxBytes < video.MaxVideoBytes {
		return errors.New("capabilities.video.storage_max_bytes 不能小于 max_video_bytes")
	}
	if video.PrepareTimeoutSeconds < video.DownloadTimeoutSeconds {
		return errors.New("capabilities.video.prepare_timeout_seconds 不能小于 download_timeout_seconds")
	}
	return nil
}

func defaultPositiveInt(target *int, fallback int) {
	if *target <= 0 {
		*target = fallback
	}
}

func defaultPositiveInt64(target *int64, fallback int64) {
	if *target <= 0 {
		*target = fallback
	}
}

func defaultVideoPath(value, fallback string) string {
	if value == "" {
		value = fallback
	}
	return filepath.Clean(value)
}
