package main

// Config 插件配置（宿主首次加载时自动把默认值写回 plugins/config.toml）
type Config struct {
	BaseURL             string   `toml:"base_url" comment:"Hermes gateway API 地址（本地源码运行的 gateway）"`
	APIKey              string   `toml:"api_key" comment:"Hermes API Server 密钥（.env 的 API_SERVER_KEY，至少16字符）"`
	FastProfile         string   `toml:"fast_profile" comment:"快速通道 profile 名（空字符串=不启用 profile 多路）"`
	FullProfile         string   `toml:"full_profile" comment:"任务通道 profile 名"`
	VisionProfile       string   `toml:"vision_profile" comment:"视觉通道 profile 名（带图消息与表情打标走它；须配视觉模型；空=跟随原通道）"`
	FullKeywords        []string `toml:"full_keywords" comment:"消息命中任一关键词则路由到任务通道"`
	ContextWindow       int      `toml:"context_window" comment:"每个会话保留的最近消息条数"`
	ContextMaxChars     int      `toml:"context_max_chars" comment:"注入提示的群上下文块最大字符数"`
	AmbientReplyRate    float64  `toml:"ambient_reply_rate" comment:"群内未@时随机参与概率(0-1)，0=只响应@与引用"`
	AmbientCooldownSec  int      `toml:"ambient_cooldown_sec" comment:"同群两次环境参与的最小间隔秒"`
	FreeResponseChats   []string `toml:"free_response_chats" comment:"免@响应白名单（群wxid列表）"`
	MinAmbientLen       int      `toml:"min_ambient_len" comment:"环境参与要求的最小消息字符数"`
	RunTimeoutSec       int      `toml:"run_timeout_sec" comment:"单轮 Agent 运行超时秒"`
	OutboundSlots       int      `toml:"outbound_slots" comment:"出站消息并发槽位"`
	MaxParallelSessions int      `toml:"max_parallel_sessions" comment:"同时运行的会话数上限"`
	MinReplyIntervalSec int      `toml:"min_reply_interval_sec" comment:"同会话两次回复最小间隔秒（防刷屏）"`
	MaxReplyChars       int      `toml:"max_reply_chars" comment:"超过该长度的回复自动分段"`
	MaxSegments         int      `toml:"max_segments" comment:"单轮回复最多分段数"`
	DailyTokenCap       int      `toml:"daily_token_cap" comment:"每日token上限(约), 超限后仅响应@与私聊, 0=不限"`
	BridgeListen        string   `toml:"bridge_listen" comment:"工具桥 HTTP 监听地址（供 hermes 侧插件回呼）"`
	BridgeToken         string   `toml:"bridge_token" comment:"工具桥 Bearer 令牌，需与 hermes 侧插件配置一致"`
	ActiveWindowMin     int      `toml:"active_window_min" comment:"会话绑定有效窗口分钟（工具桥防越权）"`
	ProgressHintSec     int      `toml:"progress_hint_sec" comment:"任务超过该秒数且在调用工具时发送一次进度提示, 0=关闭"`
	StickerEnabled      bool     `toml:"sticker_enabled" comment:"启用表情包收集/打标/发送"`
	StickerProbability  float64  `toml:"sticker_probability" comment:"文本回复后按情绪附加表情的概率(0-1)"`
	StickerCooldownMin  int      `toml:"sticker_cooldown_min" comment:"同一情绪表情的使用冷却分钟"`
	StickerChatCooldownMin int   `toml:"sticker_chat_cooldown_min" comment:"同聊天任意两次表情的最小间隔分钟（硬节流，0=不限）"`
	StickerEmotions     []string `toml:"sticker_emotions" comment:"表情情绪分类集合（视觉打标与选取都用它）"`
	OwnerWxIDs          []string `toml:"owner_wxids" comment:"额外主人wxid列表（宿主Owner自动并入，无需重复填）"`
	OwnerOnlyPrivate    bool     `toml:"owner_only_private" comment:"仅主人可私聊触发（陌生人私聊不响应）"`
	OwnerBypassAmbient  bool     `toml:"owner_bypass_ambient" comment:"主人在群里的消息无需@也响应（仍受会话冷却限制）"`
	GroupSessionsPerUser bool    `toml:"group_sessions_per_user" comment:"群内按发言人拆分独立Hermes会话（最强隔离；false=整群共享会话）"`
	SessionRotateMin     int     `toml:"session_rotate_min" comment:"Hermes会话按时间分桶轮换分钟数（防会话转录无限膨胀拖慢响应；0=不轮换）"`
	ProfileEnabled       bool    `toml:"profile_enabled" comment:"启用群成员画像（消息归档+后台抽取+注入）"`
	ProfileThreshold     int     `toml:"profile_threshold" comment:"成员新增消息达到该条数触发一次画像抽取"`
	ProfileBatch         int     `toml:"profile_batch" comment:"每轮画像抽取处理的成员数"`
	ProfileInject        bool    `toml:"profile_inject" comment:"群聊时把当前发言人的画像摘要注入提示（自然带出记忆）"`
	CrowEnabled          bool    `toml:"crow_enabled" comment:"打鸣：每日随机清晨在开启的群喔喔喔（群级可覆盖）"`
	GrudgeEnabled        bool    `toml:"grudge_enabled" comment:"记仇小本本：被嫌弃入账，低频翻旧账（群级可覆盖）"`
	MoodEnabled          bool    `toml:"mood_enabled" comment:"突然的自我：深夜低概率存在主义发言后自动撤回（群级可覆盖）"`
	ChronicleEnabled     bool    `toml:"chronicle_enabled" comment:"史官总开关（出刊群在会话页⚙逐群开启）"`
	ChronicleHour        int     `toml:"chronicle_hour" comment:"正刊时刻：时(0-23)"`
	ChronicleMinute      int     `toml:"chronicle_minute" comment:"正刊时刻：分(0-59)"`
	ChronicleExtraThreshold int  `toml:"chronicle_extra_threshold" comment:"号外触发阈值：当日群消息量达到该值即时加发号外(0=关闭)"`
	ChronicleStyleMode   string  `toml:"chronicle_style_mode" comment:"文风策略: rotate=每日轮换 | fixed=固定"`
	ChronicleFixedStyle  string  `toml:"chronicle_fixed_style" comment:"fixed 模式下使用的文风名"`
	ChronicleStyles      []chronicleStyle `toml:"chronicle_styles" comment:"文风池（Web 文风坊可编辑）"`
	ImageGenEnabled      bool             `toml:"imagegen_enabled" comment:"画坊：AI生图/改图总开关（服务商密钥建议在Web监控台配置，存本地库不落此文件）"`
	ImageGenBaseURL      string           `toml:"imagegen_base_url" comment:"生图服务商 API 根地址（如 https://image.mlgb7.com/v1）"`
	ImageGenAPIKey       string           `toml:"imagegen_api_key" comment:"生图服务商密钥（留空则用 Web 监控台里配置的）"`
	ImageGenModel        string           `toml:"imagegen_model" comment:"生图模型名"`
	ImageGenSize         string           `toml:"imagegen_size" comment:"默认尺寸: auto/1024x1024/1536x1024/1024x1536"`
	ImageGenDailyCap     int              `toml:"imagegen_daily_cap" comment:"全局每日生图张数上限（0=不限；会话级可在Web覆盖）"`
	ImageGenWorkers      int              `toml:"imagegen_workers" comment:"画坊并发画手数"`
}

func defaultConfig() Config {
	return Config{
		BaseURL:             "http://127.0.0.1:8642",
		APIKey:              "",
		FastProfile:         "fast",
		FullProfile:         "full",
		VisionProfile:       "vision",
		FullKeywords:        []string{"帮我", "搜索", "查一下", "写一", "生成", "总结", "分析", "翻译", "代码", "任务", "计划", "报告"},
		ContextWindow:       60,
		ContextMaxChars:     1500,
		AmbientReplyRate:    0.10,
		AmbientCooldownSec:  300,
		FreeResponseChats:   []string{},
		MinAmbientLen:       6,
		RunTimeoutSec:       300,
		OutboundSlots:       4,
		MaxParallelSessions: 4,
		MinReplyIntervalSec: 3,
		MaxReplyChars:       1200,
		MaxSegments:         3,
		DailyTokenCap:       0,
		BridgeListen:        "127.0.0.1:8790",
		BridgeToken:         "",
		ActiveWindowMin:     30,
		ProgressHintSec:     20,
		StickerEnabled:      true,
		StickerProbability:  0.35,
		StickerCooldownMin:  10,
		StickerChatCooldownMin: 2,
		StickerEmotions:     []string{"开心", "笑哭", "狗头", "赞", "疑惑", "生气", "伤心", "无语", "可爱", "其他"},
		OwnerWxIDs:          []string{},
		OwnerOnlyPrivate:    false,
		OwnerBypassAmbient:  false,
		GroupSessionsPerUser: false,
		SessionRotateMin:     60,
		ProfileEnabled:      true,
		ProfileThreshold:    20,
		ProfileBatch:        3,
		ProfileInject:       true,
		CrowEnabled:         true,
		GrudgeEnabled:       true,
		MoodEnabled:         true,
		ChronicleEnabled:    true,
		ChronicleHour:       22,
		ChronicleMinute:     0,
		ChronicleExtraThreshold: 500,
		ChronicleStyleMode:  "rotate",
		ChronicleFixedStyle: "太史公体",
		ImageGenEnabled:  false,
		ImageGenBaseURL:  "",
		ImageGenAPIKey:   "",
		ImageGenModel:    "gpt-image-2.5",
		ImageGenSize:     "auto",
		ImageGenDailyCap: 20,
		ImageGenWorkers:  2,
		ChronicleStyles: []chronicleStyle{
			{Name: "太史公体", Hint: "以《史记》笔法行文：句式凝练文白相间，篇末必有『太史公曰』点评，点评需毒舌或深沉二选一", Enabled: true},
			{Name: "说书人体", Hint: "章回体说书人：『话说』开篇、『欲知后事且听下回分解』收尾，多设悬念，冲突用回合制描写，可拍惊堂木", Enabled: true},
			{Name: "新闻联播体", Hint: "一本正经播报摸鱼日常：『本群今日经济社会发展稳中向好』式官腔+统计数据反讽（如斗图产业同比增长230%）", Enabled: true},
			{Name: "武侠体", Hint: "江湖切口：群友皆侠客，深夜上线叫夜探手机屏，斗图叫过招，刷屏叫大战三百楼，文风快意恩仇", Enabled: true},
		},
	}
}
