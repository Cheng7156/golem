package config

import (
	"fmt"
	"net/url"
	"strings"
)

func normalizeVideoProvider(provider *VideoProviderConfig, context videoProviderContext) error {
	if err := normalizeVideoProviderIdentity(provider, context); err != nil {
		return err
	}
	if err := normalizeVideoProviderRequest(provider); err != nil {
		return err
	}
	normalizeVideoProviderModes(provider)
	if err := validateVideoProviderModes(provider); err != nil {
		return err
	}
	if err := normalizeVideoProviderResponse(provider); err != nil {
		return err
	}
	provider.Categories = normalizeCategories(provider.Categories, context.defaultCategory)
	provider.AllowedMediaHosts = normalizeHostnames(provider.AllowedMediaHosts)
	return validateVideoProviderURLs(provider)
}

func normalizeVideoProviderIdentity(provider *VideoProviderConfig, context videoProviderContext) error {
	provider.ID = strings.ToLower(strings.TrimSpace(provider.ID))
	if !providerIDPattern.MatchString(provider.ID) {
		return fmt.Errorf("capabilities.video.providers[%d].id 无效", context.index)
	}
	if _, exists := context.seen[provider.ID]; exists {
		return fmt.Errorf("视频 Provider ID 重复: %s", provider.ID)
	}
	context.seen[provider.ID] = struct{}{}
	provider.Driver = strings.ToLower(strings.TrimSpace(provider.Driver))
	if provider.Driver == "" {
		provider.Driver = "http"
	}
	if provider.Driver != "http" {
		return fmt.Errorf("视频 Provider %s 使用了不支持的 driver: %s", provider.ID, provider.Driver)
	}
	return nil
}

func normalizeVideoProviderRequest(provider *VideoProviderConfig) error {
	parsed, err := parseStaticHTTPSURL(provider.Endpoint)
	if err != nil {
		return fmt.Errorf("视频 Provider %s endpoint %w", provider.ID, err)
	}
	provider.Endpoint = parsed.String()
	provider.Method = strings.ToUpper(strings.TrimSpace(provider.Method))
	if provider.Method == "" {
		provider.Method = "GET"
	}
	if provider.Method != "GET" && provider.Method != "POST" {
		return fmt.Errorf("视频 Provider %s method 只支持 GET 或 POST", provider.ID)
	}
	if err := validateVideoRequestBody(provider); err != nil {
		return err
	}
	return validateVideoCredentialTemplates(provider, parsed)
}

func validateVideoRequestBody(provider *VideoProviderConfig) error {
	if provider.Method == "GET" && (len(provider.Form) > 0 || len(provider.JSONBody) > 0) {
		return fmt.Errorf("视频 Provider %s 使用 GET 时不能配置 form 或 json_body", provider.ID)
	}
	if len(provider.Form) > 0 && len(provider.JSONBody) > 0 {
		return fmt.Errorf("视频 Provider %s 只能配置一种 POST body", provider.ID)
	}
	return nil
}

func normalizeVideoProviderModes(provider *VideoProviderConfig) {
	provider.RequestMode = strings.ToLower(strings.TrimSpace(provider.RequestMode))
	if provider.RequestMode == "" {
		provider.RequestMode = defaultVideoRequestMode(provider)
	}
	provider.ResponseMode = strings.ToLower(strings.TrimSpace(provider.ResponseMode))
	if provider.ResponseMode == "" {
		provider.ResponseMode = "auto"
	}
	provider.MaterializationMode = strings.ToLower(strings.TrimSpace(provider.MaterializationMode))
	if provider.MaterializationMode == "" {
		provider.MaterializationMode = defaultVideoMaterializationMode(provider.ResponseMode)
	}
	defaultPositiveInt(&provider.TimeoutSeconds, 15)
	defaultPositiveInt(&provider.RequestsPerMinute, 30)
	defaultPositiveInt64(&provider.MaxResponseBytes, 1<<20)
	if provider.Priority < 0 {
		provider.Priority = 0
	}
}

func validateVideoProviderModes(provider *VideoProviderConfig) error {
	if !oneOf(provider.RequestMode, "query", "form", "json") {
		return fmt.Errorf("视频 Provider %s request_mode 无效", provider.ID)
	}
	if provider.Method == "GET" && provider.RequestMode != "query" {
		return fmt.Errorf("视频 Provider %s 使用 GET 时 request_mode 必须是 query", provider.ID)
	}
	if provider.RequestMode == "form" && len(provider.JSONBody) > 0 {
		return fmt.Errorf("视频 Provider %s 使用 form 模式时不能配置 json_body", provider.ID)
	}
	if provider.RequestMode == "json" && len(provider.Form) > 0 {
		return fmt.Errorf("视频 Provider %s 使用 json 模式时不能配置 form", provider.ID)
	}
	if provider.RequestMode == "query" && (len(provider.Form) > 0 || len(provider.JSONBody) > 0) {
		return fmt.Errorf("视频 Provider %s 使用 query 模式时不能配置请求体", provider.ID)
	}
	if !oneOf(provider.ResponseMode, "auto", "binary", "json", "text_url") {
		return fmt.Errorf("视频 Provider %s response_mode 无效", provider.ID)
	}
	if !oneOf(provider.MaterializationMode, "lazy", "on_select") {
		return fmt.Errorf("视频 Provider %s materialization_mode 无效", provider.ID)
	}
	if provider.ResponseMode == "binary" && provider.MaterializationMode != "on_select" {
		return fmt.Errorf("视频 Provider %s 的 binary 响应必须使用 on_select", provider.ID)
	}
	return validateVideoProviderLimits(provider)
}

func validateVideoProviderLimits(provider *VideoProviderConfig) error {
	if provider.TimeoutSeconds > maximumProviderTimeout {
		return fmt.Errorf("视频 Provider %s timeout_seconds 不能大于 120", provider.ID)
	}
	if provider.RequestsPerMinute > maximumRequestsPerMinute {
		return fmt.Errorf("视频 Provider %s requests_per_minute 不能大于 600", provider.ID)
	}
	if provider.MaxResponseBytes > maximumMetadataBytes {
		return fmt.Errorf("视频 Provider %s max_response_bytes 不能大于 8 MiB", provider.ID)
	}
	return nil
}

func normalizeVideoProviderResponse(provider *VideoProviderConfig) error {
	response := &provider.Response
	response.SuccessPath = strings.TrimSpace(response.SuccessPath)
	response.SuccessValues = normalizeStrings(response.SuccessValues)
	response.ItemsPath = strings.TrimSpace(response.ItemsPath)
	response.ErrorPath = strings.TrimSpace(response.ErrorPath)
	if err := normalizeVideoResponseMappings(response); err != nil {
		return fmt.Errorf("视频 Provider %s: %w", provider.ID, err)
	}
	if provider.ResponseMode == "json" && response.URL.Path == "" {
		return fmt.Errorf("视频 Provider %s 的 JSON 响应必须配置 response.url.path", provider.ID)
	}
	if response.SuccessPath != "" && len(response.SuccessValues) == 0 {
		return fmt.Errorf("视频 Provider %s 配置 success_path 时必须配置 success_values", provider.ID)
	}
	return nil
}

func validateVideoProviderURLs(provider *VideoProviderConfig) error {
	parsed, _ := url.Parse(provider.Endpoint)
	if len(provider.AllowedMediaHosts) == 0 {
		provider.AllowedMediaHosts = []string{strings.ToLower(parsed.Hostname())}
	}
	for _, host := range provider.AllowedMediaHosts {
		if !validVideoHostname(host) {
			return fmt.Errorf("视频 Provider %s 的 allowed_media_hosts 包含无效主机名 %s", provider.ID, host)
		}
	}
	provider.FallbackURLPolicy = strings.ToLower(strings.TrimSpace(provider.FallbackURLPolicy))
	if provider.FallbackURLPolicy == "" {
		provider.FallbackURLPolicy = "page_url"
	}
	if !oneOf(provider.FallbackURLPolicy, "page_url", "media_url", "provider_public_url", "none") {
		return fmt.Errorf("视频 Provider %s fallback_url_policy 无效", provider.ID)
	}
	if provider.FallbackURLPolicy != "provider_public_url" {
		return nil
	}
	publicURL, err := parseStaticHTTPSURL(provider.PublicFallbackURL)
	if err != nil {
		return fmt.Errorf("视频 Provider %s public_fallback_url %w", provider.ID, err)
	}
	provider.PublicFallbackURL = publicURL.String()
	return nil
}

func validVideoHostname(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "/:@?#[]") {
		return false
	}
	parsed, err := url.Parse("https://" + value)
	return err == nil && strings.EqualFold(parsed.Hostname(), value)
}
