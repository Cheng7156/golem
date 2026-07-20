package video

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func validateHTTPRequestConfig(config HTTPProviderConfig) error {
	if config.Method != http.MethodGet && config.Method != http.MethodPost {
		return errors.New("video HTTP provider method must be GET or POST")
	}
	if !oneOf(config.RequestMode, "query", "form", "json") {
		return errors.New("video HTTP provider request mode is invalid")
	}
	if config.Method == http.MethodGet && config.RequestMode != "query" {
		return errors.New("GET video provider must use query request mode")
	}
	if config.RequestMode == "query" && (len(config.Form) > 0 || len(config.JSONBody) > 0) {
		return errors.New("query video provider cannot configure a request body")
	}
	if config.RequestMode == "form" && len(config.JSONBody) > 0 {
		return errors.New("form video provider cannot configure json_body")
	}
	if config.RequestMode == "json" && len(config.Form) > 0 {
		return errors.New("JSON video provider cannot configure form fields")
	}
	return nil
}

func validateHTTPResponseConfig(config HTTPProviderConfig) error {
	if !oneOf(config.ResponseMode, "auto", "binary", "json", "text_url") {
		return errors.New("video HTTP provider response mode is invalid")
	}
	if !oneOf(config.MaterializationMode, "lazy", "on_select") {
		return errors.New("video HTTP provider materialization mode is invalid")
	}
	if config.ResponseMode == "binary" && config.MaterializationMode != "on_select" {
		return errors.New("binary video provider must use on_select materialization")
	}
	if config.ResponseMode == "json" && strings.TrimSpace(config.Response.URL.Path) == "" {
		return errors.New("JSON video provider requires a URL response path")
	}
	if config.Response.SuccessPath != "" && len(config.Response.SuccessValues) == 0 {
		return errors.New("video provider success values are required")
	}
	return validateResponseTransforms(config.Response)
}

func validateResponseTransforms(response ResponseMapping) error {
	fields := []FieldMapping{response.URL, response.Title, response.PageURL}
	for _, field := range fields {
		if err := validateTransformSet(field.Transforms, false); err != nil {
			return err
		}
	}
	return validateTransformSet(response.Duration.Transforms, true)
}

func validateTransformSet(transforms []string, duration bool) error {
	for _, transform := range transforms {
		value := strings.ToLower(strings.TrimSpace(transform))
		if oneOf(value, TransformTrim, TransformMarkdownLinkTarget, TransformHTMLUnescape, TransformURLDecode) {
			continue
		}
		if duration && value == TransformMilliseconds {
			continue
		}
		return fmt.Errorf("unsupported video response transform %q", transform)
	}
	return nil
}

func validateHTTPTemplates(config HTTPProviderConfig) error {
	context := templateContext{
		request:   DiscoveryRequest{Query: "query", Category: "general", Limit: 1, Page: 1},
		lookupEnv: func(string) (string, bool) { return "secret", true },
	}
	sets := []map[string]string{config.Headers, config.MediaHeaders, config.Query, config.Form}
	for _, values := range sets {
		for name, template := range values {
			if strings.TrimSpace(name) == "" {
				return errors.New("video provider request field name is empty")
			}
			if _, _, err := expandTemplate(template, context); err != nil {
				return fmt.Errorf("invalid video provider field %q: %w", name, err)
			}
		}
	}
	_, _, err := expandJSONMap(config.JSONBody, context)
	return err
}
