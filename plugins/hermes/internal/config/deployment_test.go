package config_test

import (
	"path/filepath"
	"testing"

	"golem_plugin_hermes/internal/config"

	"github.com/pelletier/go-toml/v2"
)

func TestAPiHzDeploymentTOMLDecodesAndNormalizes(t *testing.T) {
	t.Parallel()
	value := config.Default()
	data := []byte(`
[capabilities]
environment_file = "data/hermes/capabilities.env"
shared_token_env = "GOLEM_CAPABILITIES_TOKEN"

[agent]
silence_rules_file = "/opt/software/wechat/data/hermes/workspace/silence-rules.txt"

[capabilities.sticker]
enabled = true
default_provider = "apihz"
max_candidates = 5
candidate_ttl_seconds = 300
max_media_bytes = 4194304
materialized_cache_max_bytes = 67108864
library_storage_max_bytes = 536870912
collection_policy = "owner"

[[capabilities.sticker.providers]]
id = "apihz"
driver = "http_json"
endpoint = "https://cn.apihz.cn/api/img/apihzbqb.php"
method = "GET"
timeout_seconds = 10
requests_per_minute = 8
max_query_runes = 10
allowed_media_hosts = ["res.apihz.cn"]

[capabilities.sticker.providers.query]
id = "${env:APIHZ_ID}"
key = "${env:APIHZ_KEY}"
type = "2"
limit = "${limit}"
words = "${query}"
page = "${page}"

[capabilities.sticker.providers.response]
success_path = "code"
success_values = ["200"]
items_path = "res"
error_path = "msg"

[capabilities.sticker.providers.response.url]
transforms = ["trim", "markdown_link_target"]
`)
	if err := toml.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode deployment TOML: %v", err)
	}
	normalized, err := config.Normalize(value)
	if err != nil {
		t.Fatalf("Normalize deployment TOML: %v", err)
	}
	assertAPiHzDeployment(t, normalized)
	if normalized.Agent.SilenceRulesFile != filepath.Clean("/opt/software/wechat/data/hermes/workspace/silence-rules.txt") {
		t.Fatalf("silence rules file=%q", normalized.Agent.SilenceRulesFile)
	}
}

func assertAPiHzDeployment(t *testing.T, value config.Config) {
	t.Helper()
	if len(value.Capabilities.Sticker.Providers) != 1 {
		t.Fatalf("providers=%d", len(value.Capabilities.Sticker.Providers))
	}
	if value.Capabilities.Sticker.MaterializedCacheMaxBytes != 64<<20 {
		t.Fatalf("materialized cache bytes=%d", value.Capabilities.Sticker.MaterializedCacheMaxBytes)
	}
	if value.Capabilities.Sticker.LibraryStorageMaxBytes != 512<<20 ||
		value.Capabilities.Sticker.CollectionPolicy != "owner" {
		t.Fatalf("sticker library config=%#v", value.Capabilities.Sticker)
	}
	if value.Capabilities.EnvironmentFile != filepath.Clean("data/hermes/capabilities.env") {
		t.Fatalf("environment file=%q", value.Capabilities.EnvironmentFile)
	}
	provider := value.Capabilities.Sticker.Providers[0]
	if provider.Query["key"] != "${env:APIHZ_KEY}" || provider.Query["words"] != "${query}" || provider.Response.ItemsPath != "res" {
		t.Fatalf("decoded provider=%#v", provider)
	}
}
