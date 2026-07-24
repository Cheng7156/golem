package config

func Default() Config {
	return Config{
		DataDir:              "data/hermes",
		ShutdownGraceSeconds: 15,
		BotNames:             []string{"hermes"},
		Ingress: IngressConfig{
			ReorderWindowMilliseconds:        120,
			DurableAcceptTimeoutMilliseconds: 100,
		},
		Routing: RoutingConfig{
			SocialMode:                  "agent",
			SampleRate:                  1,
			DecisionTimeoutMilliseconds: 1800,
			DecisionContextMessages:     10,
			DecisionMinConfidence:       0.72,
			DecisionAPIKeyEnv:           "GLM_API_KEY",
			OrdinaryFreshnessSeconds:    8,
			CoalesceWindowMilliseconds:  900,
			AmbientCooldownSeconds:      20,
			AmbientWindowSeconds:        60,
			AmbientMaxReplies:           2,
		},
		Context: ContextConfig{
			Mode: "legacy_shadow", Backfill: "from_now", RecentRawMessages: 10, MaxProjectionTokens: 4000,
			BarrierWaitMilliseconds: 1500,
		},
		Scheduler: SchedulerConfig{
			RouterWorkers:              2,
			InteractiveWorkers:         4,
			InteractiveReservedWorkers: 1,
			RunAdmissionMode:           "off",
			JobWorkers:                 2,
			ToolWorkers:                8,
			MaxActiveSessions:          512,
		},
		Agent: AgentConfig{
			Mode:           "relay",
			RelayListen:    "127.0.0.1:8789",
			RelayPath:      "/relay",
			BaseURL:        "http://127.0.0.1:8000/v1/chat/completions",
			Model:          "glm-5-turbo",
			SystemPrompt:   "You are Hermes in a WeChat conversation. Reply naturally and concisely.",
			TimeoutSeconds: 120,
		},
		Output: OutputConfig{
			DeliverySemantics:        "at_least_once",
			Workers:                  4,
			MaxAttempts:              12,
			AmbiguousMaxAttempts:     2,
			ProgressMaxMessages:      8,
			SendTimeoutSeconds:       15,
			RetryMinSeconds:          1,
			RetryMaxSeconds:          300,
			SendIntervalMilliseconds: 700,
			SendJitterMilliseconds:   250,
		},
		Capabilities: defaultCapabilities(),
	}
}
