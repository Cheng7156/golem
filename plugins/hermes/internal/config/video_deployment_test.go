package config_test

import (
	"testing"

	"golem_plugin_hermes/internal/config"

	"github.com/pelletier/go-toml/v2"
)

const videoProviderTOML = `
[capabilities.video]
enabled = true
link_fallback_enabled = false
max_source_bytes = 67108864
max_video_bytes = 25165824

[[capabilities.video.providers]]
id = "beauty_json"
endpoint = "https://api.example.com/videos"
method = "POST"
request_mode = "json"
response_mode = "json"
categories = ["beauty"]
allowed_media_hosts = ["cdn.example.com"]

[capabilities.video.providers.headers]
Authorization = "Bearer ${env:VIDEO_API_KEY}"

[capabilities.video.providers.json_body]
limit = 3
enabled = true

[capabilities.video.providers.json_body.filters]
query = "${query}"
category = "${category}"

[capabilities.video.providers.response]
items_path = "data.items"

[capabilities.video.providers.response.url]
path = "play_url"
transforms = ["trim"]

[[capabilities.video.providers]]
id = "random_binary"
endpoint = "https://video.example.com/random"
response_mode = "binary"
categories = ["funny"]
`

func TestVideoProviderTOMLDecodesDiverseResponseAndRequestFormats(t *testing.T) {
	value := config.Default()
	if err := toml.Unmarshal([]byte(videoProviderTOML), &value); err != nil {
		t.Fatalf("decode video TOML: %v", err)
	}
	normalized, err := config.Normalize(value)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	assertVideoProviderConfig(t, normalized.Capabilities.Video)
}

func assertVideoProviderConfig(t *testing.T, video config.VideoCapabilityConfig) {
	t.Helper()
	if video.LinkFallbackEnabled {
		t.Fatal("explicit link_fallback_enabled=false was overwritten")
	}
	if video.MaxSourceBytes != 64<<20 || video.MaxVideoBytes != 24<<20 {
		t.Fatalf("video byte limits=%#v", video)
	}
	providers := video.Providers
	if len(providers) != 2 || providers[1].MaterializationMode != "on_select" {
		t.Fatalf("providers=%#v", providers)
	}
	filters, ok := providers[0].JSONBody["filters"].(map[string]any)
	if !ok || filters["query"] != "${query}" || providers[0].JSONBody["limit"] != int64(3) {
		t.Fatalf("json_body=%#v", providers[0].JSONBody)
	}
}
