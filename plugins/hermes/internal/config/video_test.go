package config

import (
	"path/filepath"
	"testing"
)

func TestNormalizeVideoDefaults(t *testing.T) {
	value, err := Normalize(Default())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	video := value.Capabilities.Video
	if video.MaxSourceBytes != 64<<20 || video.MaxVideoBytes != 24<<20 || video.MaxVideosPerRun != 3 {
		t.Fatalf("video defaults=%#v", video)
	}
	if video.MediaDirectory != filepath.Clean("data/hermes/media/video") || !video.LinkFallbackEnabled {
		t.Fatalf("video storage defaults=%#v", video)
	}
}

func TestVideoProviderCloneIsImmutable(t *testing.T) {
	value := Default()
	value.Capabilities.Video.Providers = []VideoProviderConfig{{
		ID: "json_api", Endpoint: "https://api.example.com/video", ResponseMode: "json",
		Headers:  map[string]string{"X-Test": "one"},
		Response: VideoResponseConfig{URL: VideoFieldMapping{Path: "data.url", Transforms: []string{"trim"}}},
	}}
	manager, err := NewManager(value)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	first := manager.Current()
	first.Capabilities.Video.Providers[0].Headers["X-Test"] = "changed"
	first.Capabilities.Video.Providers[0].Response.URL.Transforms[0] = "changed"
	second := manager.Current().Capabilities.Video.Providers[0]
	if second.Headers["X-Test"] != "one" || second.Response.URL.Transforms[0] != "trim" {
		t.Fatalf("video provider snapshot was mutated: %#v", second)
	}
}

func TestNormalizeVideoProviderModes(t *testing.T) {
	value := Default()
	value.Capabilities.Video.Enabled = true
	value.Capabilities.Video.Providers = []VideoProviderConfig{
		{
			ID: "Funny_API", Endpoint: "https://api.example.com/video",
			Categories: []string{"Funny"}, ResponseMode: "json",
			Response: VideoResponseConfig{URL: VideoFieldMapping{Path: "data.url"}},
		},
		{
			ID: "binary_api", Endpoint: "https://binary.example.com/random",
			ResponseMode: "binary",
		},
	}
	normalized, err := Normalize(value)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	jsonProvider := normalized.Capabilities.Video.Providers[0]
	if jsonProvider.ID != "funny_api" || jsonProvider.MaterializationMode != "lazy" {
		t.Fatalf("json provider=%#v", jsonProvider)
	}
	binaryProvider := normalized.Capabilities.Video.Providers[1]
	if binaryProvider.MaterializationMode != "on_select" || len(binaryProvider.AllowedMediaHosts) != 1 {
		t.Fatalf("binary provider=%#v", binaryProvider)
	}
}

func TestNormalizeVideoRejectsWeChatOutputOverflow(t *testing.T) {
	value := Default()
	value.Capabilities.Video.MaxVideoBytes = 26 << 20
	if _, err := Normalize(value); err == nil {
		t.Fatal("Normalize accepted video larger than the WeChat output limit")
	}
}

func TestNormalizeVideoRequiresSourceBudgetToCoverOutput(t *testing.T) {
	value := Default()
	value.Capabilities.Video.MaxSourceBytes = 20 << 20
	value.Capabilities.Video.MaxVideoBytes = 24 << 20
	if _, err := Normalize(value); err == nil {
		t.Fatal("Normalize accepted source budget below output budget")
	}
}

func TestNormalizeVideoRequiresRelayMode(t *testing.T) {
	value := Default()
	value.Agent.Mode = "http"
	value.Agent.BaseURL = "https://example.com/v1"
	value.Capabilities.Video.Enabled = true
	if _, err := Normalize(value); err == nil {
		t.Fatal("Normalize accepted video capability outside relay mode")
	}
}
