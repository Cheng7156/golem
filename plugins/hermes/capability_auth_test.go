package main

import (
	"os"
	"path/filepath"
	"testing"

	"golem_plugin_hermes/internal/config"
)

func TestLoadCapabilityEnvironmentForPluginReload(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "capabilities.env")
	data := []byte("# plugin-local credentials\nGOLEM_CAPABILITIES_TOKEN=plugin-token-long-enough\nAPIHZ_KEY=provider-key\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	values, err := loadCapabilityEnvironment(path)
	if err != nil {
		t.Fatalf("loadCapabilityEnvironment: %v", err)
	}
	if values["APIHZ_KEY"] != "provider-key" {
		t.Fatalf("values=%#v", values)
	}
}

func TestCapabilityEnvironmentLookupPrefersPluginFile(t *testing.T) {
	t.Setenv("APIHZ_KEY", "host-environment-key")
	lookup := capabilityEnvironmentLookup(capabilityEnvironment{
		"APIHZ_KEY": "plugin-file-key",
	})

	value, exists := lookup("APIHZ_KEY")
	if !exists || value != "plugin-file-key" {
		t.Fatalf("value=%q exists=%v", value, exists)
	}
}

func TestCapabilityEnvironmentLookupSupportsLegacyHostEnvironment(t *testing.T) {
	t.Setenv("APIHZ_KEY", "host-environment-key")
	value, exists := capabilityEnvironmentLookup(nil)("APIHZ_KEY")
	if !exists || value != "host-environment-key" {
		t.Fatalf("value=%q exists=%v", value, exists)
	}
}

func TestResolveCapabilityTokenUsesPluginEnvironment(t *testing.T) {
	value := config.Default().Capabilities
	lookup := capabilityEnvironmentLookup(capabilityEnvironment{
		"GOLEM_CAPABILITIES_TOKEN": "plugin-token-long-enough",
	})

	token, err := resolveCapabilityToken(value, lookup)
	if err != nil {
		t.Fatalf("resolveCapabilityToken: %v", err)
	}
	if token != "plugin-token-long-enough" {
		t.Fatalf("token=%q", token)
	}
}

func TestLoadCapabilityEnvironmentRejectsMalformedLine(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "capabilities.env")
	if err := os.WriteFile(path, []byte("invalid-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadCapabilityEnvironment(path); err == nil {
		t.Fatal("expected malformed environment file to be rejected")
	}
}

func TestDisabledStickerCapabilityDoesNotReadEnvironmentFile(t *testing.T) {
	value := config.Default().Capabilities
	value.EnvironmentFile = filepath.Join(t.TempDir(), "missing.env")

	stickers, token, err := buildStickerCapability(value)
	if err != nil {
		t.Fatalf("buildStickerCapability: %v", err)
	}
	if stickers != nil || token != "" {
		t.Fatalf("stickers=%#v token=%q", stickers, token)
	}
}

func TestAsyncDeliveryLoadsTokenWithoutStickerCapability(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "capabilities.env")
	if err := os.WriteFile(
		path, []byte("GOLEM_CAPABILITIES_TOKEN=async-token-long-enough\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	value := config.Default().Capabilities
	value.EnvironmentFile = path
	stickers, token, err := buildRelayCapabilities(value, true)
	if err != nil {
		t.Fatalf("buildRelayCapabilities: %v", err)
	}
	if stickers != nil || token != "async-token-long-enough" {
		t.Fatalf("stickers=%#v token=%q", stickers, token)
	}
}

func TestVideoCapabilityLoadsTokenWithoutStickerCapability(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "capabilities.env")
	if err := os.WriteFile(
		path, []byte("GOLEM_CAPABILITIES_TOKEN=video-token-long-enough\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	value := config.Default().Capabilities
	value.EnvironmentFile = path
	value.Video.Enabled = true
	stickers, token, err := buildRelayCapabilities(value, false)
	if err != nil {
		t.Fatalf("buildRelayCapabilities: %v", err)
	}
	if stickers != nil || token != "video-token-long-enough" {
		t.Fatalf("stickers=%#v token=%q", stickers, token)
	}
}
