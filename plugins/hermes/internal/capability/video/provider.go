package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultMetadataBytes int64 = 1 << 20
	defaultSourceBytes   int64 = 64 << 20
	maximumRedirects           = 5
)

type httpProvider struct {
	config    HTTPProviderConfig
	client    *http.Client
	lookupEnv func(string) (string, bool)
	limiter   *minuteLimiter
	hosts     map[string]struct{}
}

type candidateReference struct {
	Deferred bool             `json:"deferred,omitempty"`
	Request  DiscoveryRequest `json:"request,omitempty"`
	MediaURL string           `json:"media_url,omitempty"`
}

func NewHTTPProvider(config HTTPProviderConfig, options ...HTTPProviderOption) (Provider, error) {
	normalizeHTTPConfig(&config)
	if err := validateHTTPConfig(config); err != nil {
		return nil, err
	}
	provider := &httpProvider{
		config:    cloneHTTPConfig(config),
		client:    &http.Client{Timeout: config.Timeout},
		lookupEnv: os.LookupEnv,
		limiter:   newMinuteLimiter(config.RequestsPerMinute),
		hosts:     allowedHostSet(config),
	}
	for _, option := range options {
		option(provider)
	}
	provider.client = provider.secureClient(provider.client)
	return provider, nil
}

func normalizeHTTPConfig(config *HTTPProviderConfig) {
	config.ProviderID = strings.TrimSpace(config.ProviderID)
	config.Method = strings.ToUpper(strings.TrimSpace(config.Method))
	config.RequestMode = strings.ToLower(strings.TrimSpace(config.RequestMode))
	config.ResponseMode = strings.ToLower(strings.TrimSpace(config.ResponseMode))
	config.MaterializationMode = strings.ToLower(strings.TrimSpace(config.MaterializationMode))
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	if config.RequestsPerMinute <= 0 {
		config.RequestsPerMinute = 30
	}
	if config.MaxMetadataBytes <= 0 {
		config.MaxMetadataBytes = defaultMetadataBytes
	}
	if config.MaxSourceBytes <= 0 {
		config.MaxSourceBytes = defaultSourceBytes
	}
}

func validateHTTPConfig(config HTTPProviderConfig) error {
	if config.ProviderID == "" {
		return errors.New("video HTTP provider id is empty")
	}
	if err := validateHTTPRequestConfig(config); err != nil {
		return err
	}
	if err := validateHTTPResponseConfig(config); err != nil {
		return err
	}
	if err := validateHTTPTemplates(config); err != nil {
		return err
	}
	return validateProviderURL(config)
}

func validateProviderURL(config HTTPProviderConfig) error {
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("video HTTP provider endpoint is invalid")
	}
	if parsed.Scheme != "https" && !(config.AllowHTTP && parsed.Scheme == "http") {
		return errors.New("video HTTP provider endpoint must use HTTPS")
	}
	if len(config.AllowedMediaHosts) == 0 {
		return errors.New("video HTTP provider media host allowlist is empty")
	}
	return nil
}

func (p *httpProvider) ID() string {
	return p.config.ProviderID
}

func (p *httpProvider) Categories() []string {
	return append([]string(nil), p.config.Categories...)
}

func (p *httpProvider) Discover(ctx context.Context, input DiscoveryRequest) ([]ProviderCandidate, error) {
	input = normalizeDiscoveryRequest(input)
	if p.config.MaterializationMode == "on_select" {
		return p.deferredCandidate(input)
	}
	response, secrets, err := p.execute(ctx, input)
	if err != nil {
		return nil, err
	}
	decoded, err := p.decodeResponse(decodeResponseRequest{
		response: response, secrets: secrets, input: input,
	})
	if err != nil {
		return nil, err
	}
	return limitCandidates(decoded.candidates, input.Limit), nil
}

func (p *httpProvider) Resolve(ctx context.Context, candidate ProviderCandidate) (ResolvedVideo, error) {
	reference, err := parseCandidateReference(candidate.Reference)
	if err != nil {
		return ResolvedVideo{}, err
	}
	if !reference.Deferred {
		return p.resolveURLCandidate(candidate, reference)
	}
	response, secrets, err := p.execute(ctx, normalizeDiscoveryRequest(reference.Request))
	if err != nil {
		return ResolvedVideo{}, err
	}
	decoded, err := p.decodeResponse(decodeResponseRequest{
		response: response, secrets: secrets, input: reference.Request, allowBinary: true,
	})
	if err != nil {
		return ResolvedVideo{}, err
	}
	resolved, err := p.firstResolved(decoded)
	if err != nil {
		return ResolvedVideo{}, err
	}
	resolved.FallbackURL = p.resolvedFallback(candidate, reference, resolved)
	return resolved, nil
}

func (p *httpProvider) execute(ctx context.Context, input DiscoveryRequest) (*http.Response, []string, error) {
	if !p.limiter.Allow(time.Now()) {
		return nil, nil, fmt.Errorf("video provider %q request limit exceeded", p.ID())
	}
	built, err := p.buildRequest(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	response, err := p.client.Do(built.request)
	if err != nil {
		return nil, nil, fmt.Errorf("video provider %q request failed: %w", p.ID(), err)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, built.secrets, nil
	}
	defer response.Body.Close()
	message, _ := readTextLimit(response.Body, p.config.MaxMetadataBytes)
	return nil, nil, &HTTPStatusError{
		ProviderID: p.ID(), StatusCode: response.StatusCode, Message: redact(message, built.secrets),
	}
}

func (p *httpProvider) deferredCandidate(input DiscoveryRequest) ([]ProviderCandidate, error) {
	reference, err := json.Marshal(candidateReference{Deferred: true, Request: input})
	if err != nil {
		return nil, fmt.Errorf("encode deferred video candidate: %w", err)
	}
	title := strings.TrimSpace(input.Query)
	if title == "" {
		title = strings.TrimSpace(input.Category) + " 视频"
	}
	return []ProviderCandidate{{Reference: string(reference), Title: title}}, nil
}

func parseCandidateReference(value string) (candidateReference, error) {
	var reference candidateReference
	if json.Unmarshal([]byte(value), &reference) != nil {
		return candidateReference{}, ErrInvalidReference
	}
	if !reference.Deferred && strings.TrimSpace(reference.MediaURL) == "" {
		return candidateReference{}, ErrInvalidReference
	}
	return reference, nil
}

func limitCandidates(values []ProviderCandidate, limit int) []ProviderCandidate {
	if limit > 0 && len(values) > limit {
		return values[:limit]
	}
	return values
}
