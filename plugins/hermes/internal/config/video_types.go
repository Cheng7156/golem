package config

type VideoCapabilityConfig struct {
	Enabled                  bool                  `toml:"enabled" comment:"是否向 Hermes 暴露视频能力"`
	DefaultCategory          string                `toml:"default_category" comment:"未指定分类时使用的默认分类"`
	MaxCandidates            int                   `toml:"max_candidates" comment:"单次发现最多返回的候选数"`
	CandidateTTLSeconds      int                   `toml:"candidate_ttl_seconds" comment:"候选 ID 有效期秒数"`
	MaxSourceBytes           int64                 `toml:"max_source_bytes" comment:"下载源视频最大字节数"`
	MaxVideoBytes            int64                 `toml:"max_video_bytes" comment:"发送成品视频最大字节数"`
	MaxDurationSeconds       int                   `toml:"max_duration_seconds" comment:"单个视频最大时长秒数"`
	MaxVideosPerRun          int                   `toml:"max_videos_per_run" comment:"单个 Run 最多暂存的视频数"`
	PrepareTimeoutSeconds    int                   `toml:"prepare_timeout_seconds" comment:"下载与处理总超时秒数"`
	DownloadTimeoutSeconds   int                   `toml:"download_timeout_seconds" comment:"单次远程下载超时秒数"`
	PrepareWorkers           int                   `toml:"prepare_workers" comment:"视频准备任务并发数"`
	StorageMaxBytes          int64                 `toml:"storage_max_bytes" comment:"视频媒体对象总磁盘预算"`
	CacheTTLHours            int                   `toml:"cache_ttl_hours" comment:"无引用媒体对象保留小时数"`
	FFmpegPath               string                `toml:"ffmpeg_path" comment:"ffmpeg 可执行文件路径"`
	FFprobePath              string                `toml:"ffprobe_path" comment:"ffprobe 可执行文件路径"`
	MediaDirectory           string                `toml:"media_directory" comment:"视频媒体对象目录"`
	LinkFallbackEnabled      bool                  `toml:"link_fallback_enabled" comment:"准备失败后是否发送原因和网页链接"`
	URLFetchAllowHTTP        bool                  `toml:"url_fetch_allow_http" comment:"任意 URL 抓取是否允许公共 HTTP 地址"`
	URLInspectTimeoutSeconds int                   `toml:"url_inspect_timeout_seconds" comment:"任意 URL 探测超时秒数"`
	URLInspectMaxBytes       int64                 `toml:"url_inspect_max_bytes" comment:"任意 URL JSON/文本探测最大字节数"`
	Providers                []VideoProviderConfig `toml:"providers" comment:"可配置视频 API Provider"`
}

type VideoProviderConfig struct {
	ID                  string              `toml:"id" comment:"Provider 唯一 ID"`
	Driver              string              `toml:"driver" comment:"Provider 驱动，当前支持 http"`
	Disabled            bool                `toml:"disabled,omitempty" comment:"是否禁用此 Provider"`
	Categories          []string            `toml:"categories" comment:"Provider 可处理的视频分类"`
	Priority            int                 `toml:"priority" comment:"同分类 Provider 优先级"`
	Endpoint            string              `toml:"endpoint" comment:"Provider HTTPS 地址"`
	Method              string              `toml:"method" comment:"GET 或 POST"`
	RequestMode         string              `toml:"request_mode" comment:"query、form 或 json"`
	ResponseMode        string              `toml:"response_mode" comment:"auto、binary、json 或 text_url"`
	MaterializationMode string              `toml:"materialization_mode" comment:"lazy 或 on_select"`
	TimeoutSeconds      int                 `toml:"timeout_seconds" comment:"单次 Provider 请求超时秒数"`
	RequestsPerMinute   int                 `toml:"requests_per_minute" comment:"本地每分钟请求上限"`
	MaxResponseBytes    int64               `toml:"max_response_bytes" comment:"非视频响应最大字节数"`
	Headers             map[string]string   `toml:"headers,omitempty" comment:"请求头模板"`
	MediaHeaders        map[string]string   `toml:"media_headers,omitempty" comment:"下载视频时使用的请求头模板"`
	Query               map[string]string   `toml:"query,omitempty" comment:"URL 查询参数模板"`
	Form                map[string]string   `toml:"form,omitempty" comment:"表单参数模板"`
	JSONBody            map[string]any      `toml:"json_body,omitempty" comment:"支持嵌套结构的 JSON Body 模板"`
	AllowedMediaHosts   []string            `toml:"allowed_media_hosts" comment:"视频下载域名白名单"`
	PublicFallbackURL   string              `toml:"public_fallback_url,omitempty" comment:"Provider 公共回退页面"`
	FallbackURLPolicy   string              `toml:"fallback_url_policy" comment:"page_url、media_url、provider_public_url 或 none"`
	Response            VideoResponseConfig `toml:"response" comment:"Provider 响应映射"`
}

type VideoResponseConfig struct {
	SuccessPath   string            `toml:"success_path,omitempty"`
	SuccessValues []string          `toml:"success_values,omitempty"`
	ItemsPath     string            `toml:"items_path,omitempty"`
	ErrorPath     string            `toml:"error_path,omitempty"`
	URL           VideoFieldMapping `toml:"url"`
	Title         VideoFieldMapping `toml:"title"`
	PageURL       VideoFieldMapping `toml:"page_url"`
	Duration      VideoFieldMapping `toml:"duration"`
}

type VideoFieldMapping struct {
	Path       string   `toml:"path,omitempty"`
	Transforms []string `toml:"transforms,omitempty"`
}
