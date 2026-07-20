package config_test

import (
	"path/filepath"
	"testing"

	"golem_plugin_hermes/internal/config"
)

func TestManagerPublishesValidatedImmutableSnapshots(t *testing.T) {
	t.Parallel()
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	first := manager.Current()
	if first == nil || first.Version != 1 {
		t.Fatalf("unexpected first snapshot: %#v", first)
	}
	first.Routing.SocialMode = "observe"

	current := manager.Current()
	if current.Routing.SocialMode != "agent" {
		t.Fatalf("snapshot was mutated through caller copy: %s", current.Routing.SocialMode)
	}

	next := current.Config
	next.Routing.SocialMode = "agent"
	next.Routing.SampleRate = 0.5
	published, err := manager.Publish(next)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Version != 2 || manager.Current().Routing.SocialMode != "agent" {
		t.Fatalf("unexpected published snapshot: %#v", published)
	}
}

func TestNormalizeRejectsUnsafeOrContradictoryValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{
			name: "unknown social mode",
			mutate: func(value *config.Config) {
				value.Routing.SocialMode = "sometimes"
			},
		},
		{
			name: "invalid sample rate",
			mutate: func(value *config.Config) {
				value.Routing.SampleRate = 1.1
			},
		},
		{
			name: "reserved workers consume pool",
			mutate: func(value *config.Config) {
				value.Scheduler.InteractiveReservedWorkers = value.Scheduler.InteractiveWorkers
			},
		},
		{
			name: "at most once is unsupported",
			mutate: func(value *config.Config) {
				value.Output.DeliverySemantics = "at_most_once"
			},
		},
		{
			name: "current directory data",
			mutate: func(value *config.Config) {
				value.DataDir = "."
			},
		},
		{
			name: "materialized cache smaller than one sticker",
			mutate: func(value *config.Config) {
				value.Capabilities.Sticker.MaterializedCacheMaxBytes = 1
			},
		},
		{
			name: "relay gateway id without secret",
			mutate: func(value *config.Config) {
				value.Agent.RelayGatewayID = "gateway-1"
			},
		},
		{
			name: "relay secret without gateway id",
			mutate: func(value *config.Config) {
				value.Agent.RelaySharedSecret = "secret"
			},
		},
		{
			name: "unauthenticated non-loopback relay",
			mutate: func(value *config.Config) {
				value.Agent.RelayListen = "0.0.0.0:8789"
			},
		},
		{
			name: "capability environment file is directory",
			mutate: func(value *config.Config) {
				value.Capabilities.EnvironmentFile = "."
			},
		},
		{
			name: "silence rules file is directory",
			mutate: func(value *config.Config) {
				value.Agent.SilenceRulesFile = "."
			},
		},
		{
			name: "async delivery requires relay mode",
			mutate: func(value *config.Config) {
				value.Agent.Mode = "http"
				value.Agent.AsyncDeliveryEnabled = true
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := config.Default()
			test.mutate(&value)
			if _, err := config.Normalize(value); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNormalizeCleansSilenceRulesFile(t *testing.T) {
	t.Parallel()
	value := config.Default()
	value.Agent.SilenceRulesFile = "  data/hermes/workspace/../silence-rules.txt  "

	normalized, err := config.Normalize(value)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want := filepath.Clean("data/hermes/silence-rules.txt")
	if normalized.Agent.SilenceRulesFile != want {
		t.Fatalf("silence rules file=%q, want %q", normalized.Agent.SilenceRulesFile, want)
	}
}

func TestNormalizeAppliesOutputSafetyDefaults(t *testing.T) {
	t.Parallel()
	value := config.Default()
	value.Output.Workers = 0
	value.Output.MaxAttempts = 0
	value.Output.AmbiguousMaxAttempts = 0
	normalized, err := config.Normalize(value)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	defaults := config.Default()
	if normalized.Output.Workers != defaults.Output.Workers {
		t.Fatalf("output workers=%d, want %d", normalized.Output.Workers, defaults.Output.Workers)
	}
	if normalized.Output.MaxAttempts != defaults.Output.MaxAttempts {
		t.Fatalf("output max attempts=%d, want %d", normalized.Output.MaxAttempts, defaults.Output.MaxAttempts)
	}
	if normalized.Output.AmbiguousMaxAttempts != defaults.Output.AmbiguousMaxAttempts {
		t.Fatalf(
			"ambiguous max attempts=%d, want %d",
			normalized.Output.AmbiguousMaxAttempts,
			defaults.Output.AmbiguousMaxAttempts,
		)
	}
}

func TestNormalizeStickerProviderAndCloneSnapshot(t *testing.T) {
	t.Parallel()
	value := config.Default()
	value.Capabilities.Sticker.Enabled = true
	value.Capabilities.Sticker.Providers = []config.StickerProviderConfig{{
		ID:                "apihz",
		Driver:            "http_json",
		Endpoint:          "https://cn.apihz.cn/api/img/apihzbqb.php",
		Method:            "post",
		Form:              map[string]string{"words": "${query}", "key": "${env:APIHZ_KEY}"},
		AllowedMediaHosts: []string{"RES.APIHZ.CN"},
		Response: config.StickerResponseConfig{
			SuccessPath:   "code",
			SuccessValues: []string{"200"},
			ItemsPath:     "res",
			ErrorPath:     "msg",
			URL: config.StickerFieldMapping{
				Transforms: []string{"TRIM", "markdown_link_target"},
			},
		},
	}}
	manager, err := config.NewManager(value)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	snapshot := manager.Current()
	if snapshot.Capabilities.SharedTokenEnv != "GOLEM_CAPABILITIES_TOKEN" {
		t.Fatalf("shared token env=%q", snapshot.Capabilities.SharedTokenEnv)
	}
	if snapshot.Capabilities.Sticker.DefaultProvider != "apihz" {
		t.Fatalf("default provider=%q", snapshot.Capabilities.Sticker.DefaultProvider)
	}
	provider := &snapshot.Capabilities.Sticker.Providers[0]
	if provider.Method != "POST" || provider.AllowedMediaHosts[0] != "res.apihz.cn" {
		t.Fatalf("provider was not normalized: %#v", provider)
	}
	provider.Form["key"] = "mutated"
	provider.Response.URL.Transforms[0] = "mutated"
	current := manager.Current().Capabilities.Sticker.Providers[0]
	if current.Form["key"] == "mutated" || current.Response.URL.Transforms[0] == "mutated" {
		t.Fatal("sticker provider snapshot shares mutable maps or slices")
	}
}

func TestNormalizeCleansCapabilityEnvironmentFile(t *testing.T) {
	t.Parallel()
	value := config.Default()
	value.Capabilities.EnvironmentFile = "  data/hermes/../hermes/capabilities.env  "

	normalized, err := config.Normalize(value)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if normalized.Capabilities.EnvironmentFile != filepath.Clean("data/hermes/capabilities.env") {
		t.Fatalf("environment file=%q", normalized.Capabilities.EnvironmentFile)
	}
}

func TestNormalizeRejectsUnsafeStickerProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*config.StickerProviderConfig)
	}{
		{name: "http endpoint", mutate: func(value *config.StickerProviderConfig) { value.Endpoint = "http://example.com/search" }},
		{name: "credentials in endpoint", mutate: func(value *config.StickerProviderConfig) { value.Endpoint = "https://user:pass@example.com/search" }},
		{name: "dynamic endpoint", mutate: func(value *config.StickerProviderConfig) { value.Endpoint = "https://example.com/${query}" }},
		{name: "raw API key in form", mutate: func(value *config.StickerProviderConfig) { value.Form["api_key"] = "must-not-be-logged" }},
		{name: "API key in endpoint query", mutate: func(value *config.StickerProviderConfig) {
			value.Endpoint = "https://example.com/search?key=must-not-be-logged"
		}},
		{name: "missing media allowlist", mutate: func(value *config.StickerProviderConfig) { value.AllowedMediaHosts = nil }},
		{name: "get with form", mutate: func(value *config.StickerProviderConfig) { value.Method = "GET" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := config.Default()
			value.Capabilities.Sticker.Enabled = true
			provider := config.StickerProviderConfig{
				ID:                "provider",
				Driver:            "http_json",
				Endpoint:          "https://example.com/search",
				Method:            "POST",
				Form:              map[string]string{"query": "${query}"},
				AllowedMediaHosts: []string{"cdn.example.com"},
				Response: config.StickerResponseConfig{
					ItemsPath: "data.items",
					URL:       config.StickerFieldMapping{Path: "url"},
				},
			}
			test.mutate(&provider)
			value.Capabilities.Sticker.Providers = []config.StickerProviderConfig{provider}
			if _, err := config.Normalize(value); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNormalizeRejectsStickerCapabilityOutsideRelayMode(t *testing.T) {
	t.Parallel()
	value := config.Default()
	value.Agent.Mode = "http"
	value.Agent.BaseURL = "https://model.example/v1"
	value.Agent.Model = "test-model"
	value.Capabilities.Sticker.Enabled = true
	value.Capabilities.Sticker.Providers = []config.StickerProviderConfig{{
		ID: "provider", Endpoint: "https://example.com/search", Method: "GET",
		Query: map[string]string{"q": "${query}"}, AllowedMediaHosts: []string{"cdn.example.com"},
		Response: config.StickerResponseConfig{ItemsPath: "items", URL: config.StickerFieldMapping{Path: "url"}},
	}}
	if _, err := config.Normalize(value); err == nil {
		t.Fatal("expected non-relay sticker capability to be rejected")
	}
}
