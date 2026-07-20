package video

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type builtRequest struct {
	request *http.Request
	secrets []string
}

func (p *httpProvider) buildRequest(ctx context.Context, input DiscoveryRequest) (builtRequest, error) {
	values := templateContext{request: input, lookupEnv: p.lookupEnv}
	requestURL, querySecrets, err := p.buildRequestURL(values)
	if err != nil {
		return builtRequest{}, err
	}
	body, contentType, bodySecrets, err := p.buildRequestBody(values)
	if err != nil {
		return builtRequest{}, err
	}
	request, err := http.NewRequestWithContext(ctx, p.config.Method, requestURL, body)
	if err != nil {
		return builtRequest{}, fmt.Errorf("create video provider request: %w", err)
	}
	secrets := append(querySecrets, bodySecrets...)
	headerSecrets, err := applyHeaderTemplates(request.Header, p.config.Headers, values)
	if err != nil {
		return builtRequest{}, err
	}
	secrets = append(secrets, headerSecrets...)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "video/*, application/json, text/plain;q=0.9")
	return builtRequest{request: request, secrets: secrets}, nil
}

func (p *httpProvider) buildRequestURL(values templateContext) (string, []string, error) {
	parsed, err := url.Parse(p.config.Endpoint)
	if err != nil {
		return "", nil, fmt.Errorf("parse video provider endpoint: %w", err)
	}
	query := parsed.Query()
	secrets, err := expandValues(query, p.config.Query, values)
	if err != nil {
		return "", nil, err
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), secrets, nil
}

func (p *httpProvider) buildRequestBody(values templateContext) (*bytes.Reader, string, []string, error) {
	if p.config.Method != http.MethodPost {
		return bytes.NewReader(nil), "", nil, nil
	}
	if p.config.RequestMode == "json" {
		return buildJSONBody(p.config.JSONBody, values)
	}
	form := make(url.Values)
	secrets, err := expandValues(form, p.config.Form, values)
	if err != nil {
		return nil, "", nil, err
	}
	return bytes.NewReader([]byte(form.Encode())), "application/x-www-form-urlencoded", secrets, nil
}

func buildJSONBody(values map[string]any, context templateContext) (*bytes.Reader, string, []string, error) {
	expanded := make(map[string]any, len(values))
	secrets := make([]string, 0)
	for name, raw := range values {
		value, found, err := expandJSONValue(raw, context)
		if err != nil {
			return nil, "", nil, fmt.Errorf("expand JSON field %q: %w", name, err)
		}
		expanded[name] = value
		secrets = append(secrets, found...)
	}
	payload, err := json.Marshal(expanded)
	if err != nil {
		return nil, "", nil, fmt.Errorf("encode video provider JSON body: %w", err)
	}
	return bytes.NewReader(payload), "application/json", secrets, nil
}

func expandJSONValue(value any, context templateContext) (any, []string, error) {
	switch item := value.(type) {
	case string:
		return expandTemplate(item, context)
	case map[string]any:
		return expandJSONMap(item, context)
	case []any:
		return expandJSONArray(item, context)
	case nil, bool, int64, float64, int, uint64:
		return item, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported JSON template value type %T", value)
	}
}

func expandJSONMap(values map[string]any, context templateContext) (map[string]any, []string, error) {
	result := make(map[string]any, len(values))
	var secrets []string
	for name, raw := range values {
		value, found, err := expandJSONValue(raw, context)
		if err != nil {
			return nil, nil, fmt.Errorf("expand nested JSON field %q: %w", name, err)
		}
		result[name] = value
		secrets = append(secrets, found...)
	}
	return result, secrets, nil
}

func expandJSONArray(values []any, context templateContext) ([]any, []string, error) {
	result := make([]any, len(values))
	var secrets []string
	for index, raw := range values {
		value, found, err := expandJSONValue(raw, context)
		if err != nil {
			return nil, nil, fmt.Errorf("expand JSON array item %d: %w", index, err)
		}
		result[index] = value
		secrets = append(secrets, found...)
	}
	return result, secrets, nil
}

func expandValues(target url.Values, templates map[string]string, context templateContext) ([]string, error) {
	secrets := make([]string, 0)
	for name, template := range templates {
		value, found, err := expandTemplate(template, context)
		if err != nil {
			return nil, fmt.Errorf("expand request field %q: %w", name, err)
		}
		target.Set(name, value)
		secrets = append(secrets, found...)
	}
	return secrets, nil
}

func applyHeaderTemplates(target http.Header, templates map[string]string, context templateContext) ([]string, error) {
	secrets := make([]string, 0)
	for name, template := range templates {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("video provider header name is empty")
		}
		value, found, err := expandTemplate(template, context)
		if err != nil {
			return nil, fmt.Errorf("expand header %q: %w", name, err)
		}
		target.Set(name, value)
		secrets = append(secrets, found...)
	}
	return secrets, nil
}
