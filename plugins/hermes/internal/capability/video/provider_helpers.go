package video

import (
	"net/http"
	"net/url"
	"strings"
)

func normalizeDiscoveryRequest(input DiscoveryRequest) DiscoveryRequest {
	input.Query = strings.TrimSpace(input.Query)
	input.Category = strings.ToLower(strings.TrimSpace(input.Category))
	if input.Limit <= 0 {
		input.Limit = 1
	}
	if input.Page <= 0 {
		input.Page = 1
	}
	return input
}

func cloneHTTPConfig(value HTTPProviderConfig) HTTPProviderConfig {
	value.Categories = append([]string(nil), value.Categories...)
	value.Headers = cloneStringMap(value.Headers)
	value.MediaHeaders = cloneStringMap(value.MediaHeaders)
	value.Query = cloneStringMap(value.Query)
	value.Form = cloneStringMap(value.Form)
	value.JSONBody = cloneJSONMap(value.JSONBody)
	value.AllowedMediaHosts = append([]string(nil), value.AllowedMediaHosts...)
	value.Response.SuccessValues = append([]string(nil), value.Response.SuccessValues...)
	value.Response.URL.Transforms = append([]string(nil), value.Response.URL.Transforms...)
	value.Response.Title.Transforms = append([]string(nil), value.Response.Title.Transforms...)
	value.Response.PageURL.Transforms = append([]string(nil), value.Response.PageURL.Transforms...)
	value.Response.Duration.Transforms = append([]string(nil), value.Response.Duration.Transforms...)
	return value
}

func cloneJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneJSONValue(item)
	}
	return result
}

func cloneJSONValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneJSONMap(item)
	case []any:
		result := make([]any, len(item))
		for index := range item {
			result[index] = cloneJSONValue(item[index])
		}
		return result
	default:
		return item
	}
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func allowedHostSet(config HTTPProviderConfig) map[string]struct{} {
	result := make(map[string]struct{}, len(config.AllowedMediaHosts)+1)
	endpoint, _ := url.Parse(config.Endpoint)
	result[normalizeHostname(endpoint.Hostname())] = struct{}{}
	for _, host := range config.AllowedMediaHosts {
		result[normalizeHostname(host)] = struct{}{}
	}
	return result
}

func normalizeHostname(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

func (p *httpProvider) secureClient(source *http.Client) *http.Client {
	client := *source
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maximumRedirects {
			return http.ErrUseLastResponse
		}
		if !p.urlAllowed(request.URL) {
			return http.ErrUseLastResponse
		}
		if len(via) > 0 && !sameOrigin(request.URL, via[0].URL) {
			stripSensitiveHeaders(request.Header)
		}
		return nil
	}
	return &client
}

func (p *httpProvider) urlAllowed(value *url.URL) bool {
	if value == nil || value.User != nil || value.Hostname() == "" {
		return false
	}
	if value.Scheme != "https" && !(p.config.AllowHTTP && value.Scheme == "http") {
		return false
	}
	_, exists := p.hosts[normalizeHostname(value.Hostname())]
	return exists
}

func sameOrigin(first, second *url.URL) bool {
	return first.Scheme == second.Scheme && strings.EqualFold(first.Host, second.Host)
}

func stripSensitiveHeaders(headers http.Header) {
	for name := range headers {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") ||
			strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "key") {
			headers.Del(name)
		}
	}
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
