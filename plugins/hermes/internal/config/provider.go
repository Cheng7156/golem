package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var sensitiveProviderFieldPattern = regexp.MustCompile(`(?i)(authorization|password|passwd|secret|token|api[-_]?key|access[-_]?key|(^|[-_])key($|[-_]))`)

func normalizeStickerProviders(sticker *StickerCapabilityConfig) error {
	seen := make(map[string]struct{}, len(sticker.Providers))
	firstEnabled := ""
	for index := range sticker.Providers {
		provider := &sticker.Providers[index]
		if err := normalizeStickerProvider(provider, index, seen); err != nil {
			return err
		}
		if !provider.Disabled && firstEnabled == "" {
			firstEnabled = provider.ID
		}
	}
	return validateStickerDefault(sticker, seen, firstEnabled)
}

func normalizeStickerProvider(
	provider *StickerProviderConfig,
	index int,
	seen map[string]struct{},
) error {
	if err := normalizeProviderIdentity(provider, index, seen); err != nil {
		return err
	}
	if err := normalizeProviderRequest(provider); err != nil {
		return err
	}
	if err := normalizeProviderLimits(provider); err != nil {
		return err
	}
	return normalizeProviderResponse(provider)
}

func normalizeProviderIdentity(
	provider *StickerProviderConfig,
	index int,
	seen map[string]struct{},
) error {
	provider.ID = strings.ToLower(strings.TrimSpace(provider.ID))
	if !providerIDPattern.MatchString(provider.ID) {
		return fmt.Errorf("capabilities.sticker.providers[%d].id 无效", index)
	}
	if _, exists := seen[provider.ID]; exists {
		return fmt.Errorf("表情 Provider ID 重复: %s", provider.ID)
	}
	seen[provider.ID] = struct{}{}
	provider.Driver = strings.ToLower(strings.TrimSpace(provider.Driver))
	if provider.Driver == "" {
		provider.Driver = "http_json"
	}
	if provider.Driver != "http_json" {
		return fmt.Errorf("表情 Provider %s 使用了不支持的 driver: %s", provider.ID, provider.Driver)
	}
	return nil
}

func normalizeProviderRequest(provider *StickerProviderConfig) error {
	parsed, err := url.Parse(strings.TrimSpace(provider.Endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return fmt.Errorf("表情 Provider %s endpoint 必须是无凭据的绝对 HTTPS URL", provider.ID)
	}
	if strings.Contains(provider.Endpoint, "${") {
		return fmt.Errorf("表情 Provider %s endpoint 必须是静态 URL；模板只能用于 query/form/header", provider.ID)
	}
	provider.Endpoint = parsed.String()
	provider.Method = strings.ToUpper(strings.TrimSpace(provider.Method))
	if provider.Method == "" {
		provider.Method = "POST"
	}
	if provider.Method != "GET" && provider.Method != "POST" {
		return fmt.Errorf("表情 Provider %s method 只支持 GET 或 POST", provider.ID)
	}
	if provider.Method == "GET" && len(provider.Form) > 0 {
		return fmt.Errorf("表情 Provider %s 使用 GET 时不能配置 form", provider.ID)
	}
	return validateProviderCredentialTemplates(provider)
}

func normalizeProviderLimits(provider *StickerProviderConfig) error {
	if provider.TimeoutSeconds <= 0 {
		provider.TimeoutSeconds = 8
	}
	if provider.TimeoutSeconds > 60 {
		return fmt.Errorf("表情 Provider %s timeout_seconds 不能大于 60", provider.ID)
	}
	if provider.RequestsPerMinute <= 0 {
		provider.RequestsPerMinute = 8
	}
	if provider.RequestsPerMinute > 600 {
		return fmt.Errorf("表情 Provider %s requests_per_minute 不能大于 600", provider.ID)
	}
	if provider.MaxQueryRunes <= 0 {
		provider.MaxQueryRunes = 32
	}
	if provider.MaxQueryRunes > 512 {
		return fmt.Errorf("表情 Provider %s max_query_runes 不能大于 512", provider.ID)
	}
	provider.AllowedMediaHosts = normalizeHostnames(provider.AllowedMediaHosts)
	if len(provider.AllowedMediaHosts) == 0 {
		return fmt.Errorf("表情 Provider %s 必须配置 allowed_media_hosts", provider.ID)
	}
	return nil
}

func normalizeProviderResponse(provider *StickerProviderConfig) error {
	response := &provider.Response
	response.SuccessPath = strings.TrimSpace(response.SuccessPath)
	response.SuccessValues = normalizeStrings(response.SuccessValues)
	response.ItemsPath = strings.TrimSpace(response.ItemsPath)
	response.ErrorPath = strings.TrimSpace(response.ErrorPath)
	response.URL.Path = strings.TrimSpace(response.URL.Path)
	response.URL.Transforms = normalizeTransforms(response.URL.Transforms)
	response.Description.Path = strings.TrimSpace(response.Description.Path)
	response.Description.Transforms = normalizeTransforms(response.Description.Transforms)
	if response.ItemsPath == "" {
		return fmt.Errorf("表情 Provider %s 必须配置 response.items_path", provider.ID)
	}
	if response.SuccessPath != "" && len(response.SuccessValues) == 0 {
		return fmt.Errorf("表情 Provider %s 配置 success_path 时必须同时配置 success_values", provider.ID)
	}
	return nil
}

func validateStickerDefault(
	sticker *StickerCapabilityConfig,
	seen map[string]struct{},
	firstEnabled string,
) error {
	if !sticker.Enabled {
		return nil
	}
	if firstEnabled == "" {
		return errors.New("启用 capabilities.sticker 时至少需要一个可用 Provider")
	}
	if sticker.DefaultProvider == "" {
		sticker.DefaultProvider = firstEnabled
	}
	sticker.DefaultProvider = strings.ToLower(sticker.DefaultProvider)
	if _, exists := seen[sticker.DefaultProvider]; !exists {
		return fmt.Errorf("默认表情 Provider 不存在: %s", sticker.DefaultProvider)
	}
	for _, provider := range sticker.Providers {
		if provider.ID == sticker.DefaultProvider && provider.Disabled {
			return fmt.Errorf("默认表情 Provider 已禁用: %s", sticker.DefaultProvider)
		}
	}
	return nil
}

func validateProviderCredentialTemplates(provider *StickerProviderConfig) error {
	parsed, _ := url.Parse(provider.Endpoint)
	for name := range parsed.Query() {
		if sensitiveProviderFieldPattern.MatchString(name) {
			return fmt.Errorf("表情 Provider %s 的敏感 URL 参数 %s 必须移入 query/form 并使用 ${env:NAME}", provider.ID, name)
		}
	}
	for _, values := range []map[string]string{provider.Headers, provider.Query, provider.Form} {
		for name, value := range values {
			if sensitiveProviderFieldPattern.MatchString(name) && !strings.Contains(value, "${env:") {
				return fmt.Errorf("表情 Provider %s 的敏感字段 %s 必须使用 ${env:NAME} 引用环境变量", provider.ID, name)
			}
		}
	}
	return nil
}

func normalizeHostnames(values []string) []string {
	result := normalizeStrings(values)
	for index := range result {
		result[index] = strings.ToLower(strings.TrimSuffix(result[index], "."))
	}
	return result
}

func normalizeTransforms(values []string) []string {
	result := normalizeStrings(values)
	for index := range result {
		result[index] = strings.ToLower(result[index])
	}
	return result
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
