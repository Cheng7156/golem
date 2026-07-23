package config

func defaultCapabilities() CapabilityConfig {
	return CapabilityConfig{
		SharedTokenEnv: "GOLEM_CAPABILITIES_TOKEN",
		Sticker: StickerCapabilityConfig{
			MaxCandidates: 5, CandidateTTLSeconds: 300,
			MaxMediaBytes: 2 << 20, MaterializedCacheMaxBytes: 64 << 20,
		},
		Video: defaultVideoCapability(),
	}
}

func defaultVideoCapability() VideoCapabilityConfig {
	return VideoCapabilityConfig{
		DefaultCategory: "general", MaxCandidates: 5, CandidateTTLSeconds: 600,
		MaxSourceBytes: 64 << 20, MaxVideoBytes: 24 << 20,
		MaxDurationSeconds: 300, MaxVideosPerRun: 3,
		PrepareTimeoutSeconds: 180, DownloadTimeoutSeconds: 60, PrepareWorkers: 2,
		StorageMaxBytes: 2 << 30, CacheTTLHours: 24,
		FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe",
		MediaDirectory: "data/hermes/media/video", LinkFallbackEnabled: true,
		URLFetchAllowHTTP: true, URLInspectTimeoutSeconds: 20, URLInspectMaxBytes: 256 << 10,
	}
}
