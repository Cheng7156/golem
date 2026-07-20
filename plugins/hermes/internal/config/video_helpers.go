package config

import (
	"fmt"
	"net/url"
	"strings"
)

func validateVideoCredentialTemplates(provider *VideoProviderConfig, endpoint *url.URL) error {
	for name := range endpoint.Query() {
		if sensitiveProviderFieldPattern.MatchString(name) {
			return fmt.Errorf("视频 Provider %s 的敏感 URL 参数 %s 必须移入请求模板", provider.ID, name)
		}
	}
	sets := []map[string]string{
		provider.Headers,
		provider.MediaHeaders,
		provider.Query,
		provider.Form,
	}
	for _, values := range sets {
		if err := validateVideoCredentialSet(provider.ID, values); err != nil {
			return err
		}
	}
	return validateVideoJSONCredentials(provider.ID, provider.JSONBody)
}

func validateVideoCredentialSet(providerID string, values map[string]string) error {
	for name, value := range values {
		if sensitiveProviderFieldPattern.MatchString(name) && !strings.Contains(value, "${env:") {
			return fmt.Errorf("视频 Provider %s 的敏感字段 %s 必须使用 ${env:NAME}", providerID, name)
		}
	}
	return nil
}

func validateVideoJSONCredentials(providerID string, values map[string]any) error {
	for name, value := range values {
		if sensitiveProviderFieldPattern.MatchString(name) {
			template, ok := value.(string)
			if !ok || !strings.Contains(template, "${env:") {
				return fmt.Errorf("视频 Provider %s 的敏感 JSON 字段 %s 必须使用 ${env:NAME}", providerID, name)
			}
		}
		if err := validateNestedVideoJSONCredentials(providerID, value); err != nil {
			return err
		}
	}
	return nil
}

func validateNestedVideoJSONCredentials(providerID string, value any) error {
	switch item := value.(type) {
	case map[string]any:
		return validateVideoJSONCredentials(providerID, item)
	case []any:
		for _, child := range item {
			if err := validateNestedVideoJSONCredentials(providerID, child); err != nil {
				return err
			}
		}
	case nil, string, bool, int, int64, uint64, float64:
		return nil
	default:
		return fmt.Errorf("视频 Provider %s 的 JSON Body 包含不支持的类型 %T", providerID, value)
	}
	return nil
}

func parseStaticHTTPSURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, fmt.Errorf("必须是无凭据的绝对 HTTPS URL")
	}
	if strings.Contains(value, "${") {
		return nil, fmt.Errorf("必须是静态 URL")
	}
	return parsed, nil
}

func normalizeVideoMapping(mapping *VideoFieldMapping) {
	mapping.Path = strings.TrimSpace(mapping.Path)
	mapping.Transforms = normalizeTransforms(mapping.Transforms)
}

func normalizeVideoResponseMappings(response *VideoResponseConfig) error {
	for _, mapping := range []*VideoFieldMapping{&response.URL, &response.Title, &response.PageURL} {
		normalizeVideoMapping(mapping)
		if err := validateVideoTransforms(mapping.Transforms, false); err != nil {
			return err
		}
	}
	normalizeVideoMapping(&response.Duration)
	return validateVideoTransforms(response.Duration.Transforms, true)
}

func validateVideoTransforms(values []string, duration bool) error {
	for _, value := range values {
		if oneOf(value, "trim", "markdown_link_target", "html_unescape", "url_decode") {
			continue
		}
		if duration && value == "milliseconds_to_seconds" {
			continue
		}
		return fmt.Errorf("不支持的视频响应转换: %s", value)
	}
	return nil
}

func normalizeCategories(values []string, fallback string) []string {
	result := make([]string, 0, len(values))
	for _, value := range normalizeStrings(values) {
		if category := normalizeCategory(value); category != "" {
			result = append(result, category)
		}
	}
	if len(result) == 0 {
		return []string{fallback}
	}
	return result
}

func normalizeCategory(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func defaultVideoRequestMode(provider *VideoProviderConfig) string {
	if provider.Method == "GET" {
		return "query"
	}
	if len(provider.JSONBody) > 0 {
		return "json"
	}
	return "form"
}

func defaultVideoMaterializationMode(responseMode string) string {
	if responseMode == "binary" || responseMode == "auto" {
		return "on_select"
	}
	return "lazy"
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
