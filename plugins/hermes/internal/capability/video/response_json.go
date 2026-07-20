package video

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

func (p *httpProvider) decodeJSON(
	body []byte,
	secrets []string,
	input DiscoveryRequest,
) (decodedResponse, error) {
	if !gjson.ValidBytes(body) {
		return decodedResponse{}, errors.New("video provider returned invalid JSON")
	}
	root := gjson.ParseBytes(body)
	if err := p.validateBusinessStatus(root, secrets); err != nil {
		return decodedResponse{}, err
	}
	items := jsonItems(root, p.config.Response.ItemsPath)
	result := make([]ProviderCandidate, 0, len(items))
	for _, item := range items {
		candidate, err := p.candidateFromJSON(item, secrets, input)
		if err != nil {
			return decodedResponse{}, err
		}
		if candidate.Reference != "" {
			result = append(result, candidate)
		}
	}
	if len(result) == 0 {
		return decodedResponse{}, ErrNoCandidates
	}
	return decodedResponse{candidates: result}, nil
}

func (p *httpProvider) validateBusinessStatus(root gjson.Result, secrets []string) error {
	mapping := p.config.Response
	if strings.TrimSpace(mapping.SuccessPath) == "" {
		return nil
	}
	code := comparableResult(resultAt(root, mapping.SuccessPath))
	for _, accepted := range mapping.SuccessValues {
		if code == strings.TrimSpace(accepted) {
			return nil
		}
	}
	message := comparableResult(resultAt(root, mapping.ErrorPath))
	return &BusinessError{ProviderID: p.ID(), Code: code, Message: redact(message, secrets)}
}

func jsonItems(root gjson.Result, path string) []gjson.Result {
	collection := resultAt(root, path)
	if collection.IsArray() {
		return collection.Array()
	}
	if !collection.Exists() && collection.Raw == "" {
		return nil
	}
	return []gjson.Result{collection}
}

func (p *httpProvider) candidateFromJSON(
	item gjson.Result,
	secrets []string,
	input DiscoveryRequest,
) (ProviderCandidate, error) {
	mapping := p.config.Response
	mediaURL, err := mappedValue(item, mapping.URL)
	if err != nil {
		return ProviderCandidate{}, err
	}
	if strings.TrimSpace(mediaURL) == "" {
		return ProviderCandidate{}, nil
	}
	mediaURL, err = p.validateMediaURL(mediaURL)
	if err != nil {
		return ProviderCandidate{}, err
	}
	title, err := mappedValue(item, mapping.Title)
	if err != nil {
		return ProviderCandidate{}, err
	}
	pageURL, err := p.mappedPageURL(item, mapping.PageURL)
	if err != nil {
		return ProviderCandidate{}, err
	}
	duration, err := mappedDuration(item, mapping.Duration)
	if err != nil {
		return ProviderCandidate{}, err
	}
	reference, err := json.Marshal(candidateReference{MediaURL: mediaURL, Request: input})
	if err != nil {
		return ProviderCandidate{}, fmt.Errorf("encode video candidate reference: %w", err)
	}
	return ProviderCandidate{
		Reference: string(reference), Title: redact(title, secrets),
		PageURL: pageURL, DurationSeconds: duration,
	}, nil
}

func mappedValue(item gjson.Result, mapping FieldMapping) (string, error) {
	if strings.TrimSpace(mapping.Path) == "" {
		return "", nil
	}
	value := comparableResult(resultAt(item, mapping.Path))
	return applyTransforms(value, mapping.Transforms)
}

func mappedDuration(item gjson.Result, mapping FieldMapping) (uint32, error) {
	value, err := mappedValue(item, mapping)
	if err != nil || strings.TrimSpace(value) == "" {
		return 0, err
	}
	seconds, err := parsePositiveFloat(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("invalid video duration %q", value)
	}
	if seconds > float64(^uint32(0)) {
		return 0, fmt.Errorf("video duration is too large")
	}
	return uint32(seconds + 0.5), nil
}

func parsePositiveFloat(value string) (float64, error) {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("expected a positive number")
	}
	return number, nil
}

func (p *httpProvider) mappedPageURL(item gjson.Result, mapping FieldMapping) (string, error) {
	value, err := mappedValue(item, mapping)
	if err != nil || strings.TrimSpace(value) == "" {
		return "", err
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("video provider returned invalid page URL")
	}
	if parsed.Scheme != "https" && !(p.config.AllowHTTP && parsed.Scheme == "http") {
		return "", errors.New("video provider page URL must use HTTPS")
	}
	return parsed.String(), nil
}

func resultAt(value gjson.Result, path string) gjson.Result {
	path = strings.TrimSpace(path)
	if path == "" || path == "$" {
		return value
	}
	return value.Get(path)
}

func comparableResult(value gjson.Result) string {
	if value.Type == gjson.String {
		return value.String()
	}
	return value.Raw
}

func (p *httpProvider) validateMediaURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !p.urlAllowed(parsed) {
		return "", fmt.Errorf("video provider returned disallowed media URL")
	}
	return parsed.String(), nil
}

func (p *httpProvider) mediaHeaders(input DiscoveryRequest) (http.Header, error) {
	headers := make(http.Header)
	_, err := applyHeaderTemplates(headers, p.config.MediaHeaders, templateContext{
		request: input, lookupEnv: p.lookupEnv,
	})
	return headers, err
}
