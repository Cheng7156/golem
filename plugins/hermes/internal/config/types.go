package config

type Config struct {
	DataDir              string           `toml:"data_dir" comment:"Hermes 数据目录"`
	ShutdownGraceSeconds int              `toml:"shutdown_grace_seconds" comment:"插件停止时等待任务收敛的秒数"`
	BotNames             []string         `toml:"bot_names,omitempty" comment:"群聊中识别机器人的名称或别名"`
	Ingress              IngressConfig    `toml:"ingress" comment:"输入持久化与重排配置"`
	Routing              RoutingConfig    `toml:"routing" comment:"普通群聊路由配置"`
	Context              ContextConfig    `toml:"context" comment:"Hermes 会话上下文投影配置"`
	Scheduler            SchedulerConfig  `toml:"scheduler" comment:"有界并发与会话容量配置"`
	Agent                AgentConfig      `toml:"agent" comment:"Agent Engine 配置"`
	Output               OutputConfig     `toml:"output" comment:"至少一次发送与重试配置"`
	Capabilities         CapabilityConfig `toml:"capabilities" comment:"由 Hermes Agent 自主调用的扩展能力"`
}

type ContextConfig struct {
	Mode                    string `toml:"mode" comment:"legacy_shadow、full 或 none"`
	Backfill                string `toml:"backfill" comment:"当前仅支持 from_now"`
	RecentRawMessages       int    `toml:"recent_raw_messages"`
	MaxProjectionTokens     int    `toml:"max_projection_tokens"`
	BarrierWaitMilliseconds int    `toml:"barrier_wait_milliseconds" comment:"交互回合等待 durable 群观察的最大毫秒数"`
}

type CapabilityConfig struct {
	EnvironmentFile string                  `toml:"environment_file,omitempty" comment:"插件加载时读取的能力凭据文件"`
	SharedTokenEnv  string                  `toml:"shared_token_env" comment:"Hermes 能力桥共享令牌的环境变量名"`
	Sticker         StickerCapabilityConfig `toml:"sticker" comment:"表情搜索与回复能力"`
	Video           VideoCapabilityConfig   `toml:"video" comment:"视频发现、准备与微信发送能力"`
}

type StickerCapabilityConfig struct {
	Enabled                   bool                    `toml:"enabled" comment:"是否向 Hermes 暴露表情能力"`
	DefaultProvider           string                  `toml:"default_provider,omitempty" comment:"默认表情 Provider ID"`
	MaxCandidates             int                     `toml:"max_candidates" comment:"单次搜索最多返回候选数"`
	CandidateTTLSeconds       int                     `toml:"candidate_ttl_seconds" comment:"候选 ID 有效期秒数"`
	MaxMediaBytes             int                     `toml:"max_media_bytes" comment:"落库前允许的最大媒体字节数"`
	MaterializedCacheMaxBytes int64                   `toml:"materialized_cache_max_bytes" comment:"已物化候选的内存缓存字节预算"`
	LibraryStorageMaxBytes    int64                   `toml:"library_storage_max_bytes" comment:"本地收藏表情的独立磁盘预算"`
	CollectionPolicy          string                  `toml:"collection_policy" comment:"收藏权限：owner 或 any"`
	Providers                 []StickerProviderConfig `toml:"providers" comment:"可替换表情来源列表"`
}

type StickerProviderConfig struct {
	ID                string                `toml:"id" comment:"Provider 唯一 ID"`
	Driver            string                `toml:"driver" comment:"Provider 驱动，当前支持 http_json"`
	Disabled          bool                  `toml:"disabled,omitempty" comment:"是否禁用此 Provider"`
	Endpoint          string                `toml:"endpoint" comment:"搜索接口 HTTPS 地址"`
	Method            string                `toml:"method" comment:"GET 或 POST"`
	TimeoutSeconds    int                   `toml:"timeout_seconds" comment:"单次请求超时秒数"`
	RequestsPerMinute int                   `toml:"requests_per_minute" comment:"本地每分钟请求上限"`
	MaxQueryRunes     int                   `toml:"max_query_runes" comment:"搜索词最大字符数"`
	Headers           map[string]string     `toml:"headers,omitempty" comment:"请求头模板"`
	Query             map[string]string     `toml:"query,omitempty" comment:"URL 查询参数模板"`
	Form              map[string]string     `toml:"form,omitempty" comment:"表单参数模板"`
	AllowedMediaHosts []string              `toml:"allowed_media_hosts" comment:"候选媒体下载域名白名单"`
	Response          StickerResponseConfig `toml:"response" comment:"JSON 响应映射"`
}

type StickerResponseConfig struct {
	SuccessPath   string              `toml:"success_path,omitempty" comment:"业务成功字段的 GJSON 路径"`
	SuccessValues []string            `toml:"success_values,omitempty" comment:"业务成功字段允许值"`
	ItemsPath     string              `toml:"items_path" comment:"候选数组的 GJSON 路径"`
	ErrorPath     string              `toml:"error_path,omitempty" comment:"业务错误信息的 GJSON 路径"`
	URL           StickerFieldMapping `toml:"url" comment:"候选 URL 字段映射"`
	Description   StickerFieldMapping `toml:"description" comment:"候选描述字段映射"`
}

type StickerFieldMapping struct {
	Path       string   `toml:"path,omitempty" comment:"相对候选项的 GJSON 路径；标量项留空"`
	Transforms []string `toml:"transforms,omitempty" comment:"安全的字段转换链"`
}

type IngressConfig struct {
	ReorderWindowMilliseconds        int `toml:"reorder_window_milliseconds"`
	DurableAcceptTimeoutMilliseconds int `toml:"durable_accept_timeout_milliseconds"`
}

type RoutingConfig struct {
	SocialMode                  string   `toml:"social_mode"`
	SampleRate                  float64  `toml:"sample_rate"`
	DecisionTimeoutMilliseconds int      `toml:"decision_timeout_milliseconds"`
	DecisionContextMessages     int      `toml:"decision_context_messages"`
	DecisionMinConfidence       float64  `toml:"decision_min_confidence"`
	DecisionBaseURL             string   `toml:"decision_base_url,omitempty"`
	DecisionModel               string   `toml:"decision_model,omitempty"`
	DecisionAPIKeyEnv           string   `toml:"decision_api_key_env,omitempty"`
	DecisionEnvironmentFile     string   `toml:"decision_environment_file,omitempty"`
	OrdinaryFreshnessSeconds    int      `toml:"ordinary_freshness_seconds"`
	CoalesceWindowMilliseconds  int      `toml:"coalesce_window_milliseconds"`
	AmbientCooldownSeconds      int      `toml:"ambient_cooldown_seconds"`
	AmbientWindowSeconds        int      `toml:"ambient_window_seconds"`
	AmbientMaxReplies           int      `toml:"ambient_max_replies"`
	AmbientMaxReplyRunes        int      `toml:"ambient_max_reply_runes" comment:"未点名群聊文本回复的最大字符数"`
	AutomatedSpeakerNames       []string `toml:"automated_speaker_names,omitempty"`
	AutomatedSpeakerIDs         []string `toml:"automated_speaker_ids,omitempty"`
}

type SchedulerConfig struct {
	RouterWorkers              int    `toml:"router_workers"`
	InteractiveWorkers         int    `toml:"interactive_workers"`
	InteractiveReservedWorkers int    `toml:"interactive_reserved_workers"`
	RunAdmissionMode           string `toml:"run_admission_mode" comment:"off、queued 或 active"`
	JobWorkers                 int    `toml:"job_workers"`
	ToolWorkers                int    `toml:"tool_workers"`
	MaxActiveSessions          int    `toml:"max_active_sessions"`
}

type AgentConfig struct {
	Mode                  string `toml:"mode" comment:"Agent Runtime：relay 或 http"`
	RelayListen           string `toml:"relay_listen" comment:"Hermes Gateway relay connector 监听地址"`
	RelayPath             string `toml:"relay_path" comment:"Hermes Gateway relay WebSocket 路径"`
	RelaySessionNamespace string `toml:"relay_session_namespace,omitempty" comment:"Relay 会话命名空间；修改后无损创建新 Hermes 会话"`
	SilenceRulesFile      string `toml:"silence_rules_file,omitempty" comment:"Relay 群聊静默回复规则文件；运行时热读取"`
	AsyncDeliveryEnabled  bool   `toml:"async_delivery_enabled" comment:"是否启用 Hermes 后台子代理持久异步回流"`
	RelayGatewayID        string `toml:"relay_gateway_id,omitempty" comment:"Hermes Gateway 鉴权 ID"`
	RelaySharedSecret     string `toml:"relay_shared_secret,omitempty" comment:"Hermes Gateway relay HMAC 密钥"`
	BaseURL               string `toml:"base_url,omitempty" comment:"HTTP 兼容模式接口地址"`
	APIKey                string `toml:"api_key,omitempty" comment:"HTTP 兼容模式密钥"`
	Model                 string `toml:"model,omitempty" comment:"HTTP 兼容模式 Agent 模型"`
	SystemPrompt          string `toml:"system_prompt,omitempty" comment:"HTTP 兼容模式系统提示词"`
	TimeoutSeconds        int    `toml:"timeout_seconds" comment:"HTTP 兼容模式单个 Run 超时秒数；relay 由 Hermes gateway_timeout 管理"`
}

type OutputConfig struct {
	DeliverySemantics        string `toml:"delivery_semantics"`
	Workers                  int    `toml:"workers"`
	MaxAttempts              int    `toml:"max_attempts"`
	AmbiguousMaxAttempts     int    `toml:"ambiguous_max_attempts"`
	SendTimeoutSeconds       int    `toml:"send_timeout_seconds"`
	RetryMinSeconds          int    `toml:"retry_min_seconds"`
	RetryMaxSeconds          int    `toml:"retry_max_seconds"`
	SendIntervalMilliseconds int    `toml:"send_interval_milliseconds"`
	SendJitterMilliseconds   int    `toml:"send_jitter_milliseconds"`
}
