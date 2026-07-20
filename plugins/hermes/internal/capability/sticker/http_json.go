package sticker

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"golem_plugin_hermes/internal/domain"
)

const defaultMaximumProviderResponseBytes int64 = 1 << 20

// HTTPJSONConfig describes a common JSON search API without embedding an
// executable expression language. Paths use GJSON syntax. For scalar array
// elements URLPath may be "$"; for object arrays it can be "url" or any other
// relative GJSON path.
type HTTPJSONConfig struct {
	ProviderID string
	Endpoint   string
	Method     string

	// Parameters become URL query parameters for GET and application/x-www-form-urlencoded
	// body fields for POST. Endpoint, parameters, and headers support ${query},
	// ${limit}, ${page}, and ${env:NAME}.
	Parameters map[string]string
	// Query is always placed in the URL. Form is allowed only for POST and is
	// encoded as application/x-www-form-urlencoded. Parameters is a convenience
	// alias for simple APIs (query for GET, form for POST); it cannot be combined
	// with Query or Form.
	Query   map[string]string
	Form    map[string]string
	Headers map[string]string

	AllowHTTPEndpoint bool
	Timeout           time.Duration
	RequestsPerMinute int
	MaxResponseBytes  int64
	MaxQueryRunes     int

	SuccessPath     string
	SuccessValues   []string
	ItemsPath       string
	URLPath         string
	DescriptionPath string
	ErrorPath       string

	URLTransforms         []string
	DescriptionTransforms []string

	MediaAllowedHosts []string
	MaxMediaBytes     int64
}

type HTTPJSONOption func(*httpJSONProvider)

func WithProviderHTTPClient(client *http.Client) HTTPJSONOption {
	return func(value *httpJSONProvider) {
		if client != nil {
			value.client = client
		}
	}
}

func WithProviderEnvironment(lookup func(string) (string, bool)) HTTPJSONOption {
	return func(value *httpJSONProvider) {
		if lookup != nil {
			value.lookupEnv = lookup
		}
	}
}

func WithProviderDownloader(downloader Downloader) HTTPJSONOption {
	return func(value *httpJSONProvider) {
		if downloader != nil {
			value.downloader = downloader
		}
	}
}

type httpJSONProvider struct {
	config     HTTPJSONConfig
	client     *http.Client
	lookupEnv  func(string) (string, bool)
	downloader Downloader
	limiter    *minuteLimiter
}

func NewHTTPJSONProvider(config HTTPJSONConfig, options ...HTTPJSONOption) (Provider, error) {
	config.ProviderID = strings.TrimSpace(config.ProviderID)
	if config.ProviderID == "" {
		return nil, errors.New("HTTP JSON sticker provider id is empty")
	}
	config.Method = strings.ToUpper(strings.TrimSpace(config.Method))
	if config.Method == "" {
		config.Method = http.MethodGet
	}
	if config.Method != http.MethodGet && config.Method != http.MethodPost {
		return nil, errors.New("HTTP JSON sticker provider method must be GET or POST")
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	if config.RequestsPerMinute <= 0 {
		config.RequestsPerMinute = 60
	}
	if config.MaxResponseBytes <= 0 {
		config.MaxResponseBytes = defaultMaximumProviderResponseBytes
	}
	if config.MaxMediaBytes <= 0 {
		config.MaxMediaBytes = defaultMaximumStickerBytes
	}
	if config.MaxQueryRunes <= 0 {
		config.MaxQueryRunes = 128
	}
	if len(config.Parameters) > 0 && (len(config.Query) > 0 || len(config.Form) > 0) {
		return nil, errors.New("HTTP JSON sticker provider parameters cannot be combined with query or form")
	}
	if config.Method == http.MethodGet && len(config.Form) > 0 {
		return nil, errors.New("HTTP JSON sticker provider form fields require POST")
	}
	if strings.TrimSpace(config.ItemsPath) == "" {
		return nil, errors.New("HTTP JSON sticker provider items_path is empty")
	}
	if strings.TrimSpace(config.URLPath) == "" {
		config.URLPath = "$"
	}
	if strings.TrimSpace(config.SuccessPath) != "" && len(config.SuccessValues) == 0 {
		return nil, errors.New("success_values are required when success_path is configured")
	}
	if err := validateTransforms(config.URLTransforms); err != nil {
		return nil, err
	}
	if err := validateTransforms(config.DescriptionTransforms); err != nil {
		return nil, err
	}
	if _, err := normalizeAllowedHosts(config.MediaAllowedHosts); err != nil {
		return nil, err
	}
	if err := validateTemplate(config.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid HTTP JSON sticker endpoint template: %w", err)
	}
	for name, template := range config.Parameters {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("HTTP JSON sticker parameter name is empty")
		}
		if err := validateTemplate(template); err != nil {
			return nil, fmt.Errorf("invalid HTTP JSON sticker parameter %q template: %w", name, err)
		}
	}
	for name, template := range config.Query {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("HTTP JSON sticker query parameter name is empty")
		}
		if err := validateTemplate(template); err != nil {
			return nil, fmt.Errorf("invalid HTTP JSON sticker query parameter %q template: %w", name, err)
		}
	}
	for name, template := range config.Form {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("HTTP JSON sticker form field name is empty")
		}
		if err := validateTemplate(template); err != nil {
			return nil, fmt.Errorf("invalid HTTP JSON sticker form field %q template: %w", name, err)
		}
	}
	for name, template := range config.Headers {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("HTTP JSON sticker header name is empty")
		}
		if err := validateTemplate(template); err != nil {
			return nil, fmt.Errorf("invalid HTTP JSON sticker header %q template: %w", name, err)
		}
	}
	// Validate endpoint shape with non-secret placeholder values. The real
	// endpoint is parsed again for every request after environment expansion.
	preview, _, err := expandTemplate(config.Endpoint, templateValues{
		query: "query",
		limit: 1,
		page:  1,
		lookupEnv: func(string) (string, bool) {
			return "secret", true
		},
	})
	if err != nil {
		return nil, err
	}
	if err := validateProviderEndpoint(preview, config.AllowHTTPEndpoint); err != nil {
		return nil, err
	}
	value := &httpJSONProvider{
		config:     cloneHTTPJSONConfig(config),
		client:     &http.Client{Timeout: config.Timeout},
		lookupEnv:  os.LookupEnv,
		downloader: NewSecureDownloader(),
		limiter:    newMinuteLimiter(config.RequestsPerMinute),
	}
	for _, option := range options {
		option(value)
	}
	// Redirects are always disabled, including when a caller injects a custom
	// client. A 307/308 would otherwise replay form bodies and authorization
	// headers containing ${env:...} credentials to a different origin.
	client := *value.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("sticker provider redirects are disabled")
	}
	value.client = &client
	return value, nil
}

func cloneHTTPJSONConfig(value HTTPJSONConfig) HTTPJSONConfig {
	clone := value
	clone.Parameters = cloneStringMap(value.Parameters)
	clone.Query = cloneStringMap(value.Query)
	clone.Form = cloneStringMap(value.Form)
	clone.Headers = cloneStringMap(value.Headers)
	clone.SuccessValues = append([]string(nil), value.SuccessValues...)
	clone.URLTransforms = append([]string(nil), value.URLTransforms...)
	clone.DescriptionTransforms = append([]string(nil), value.DescriptionTransforms...)
	clone.MediaAllowedHosts = append([]string(nil), value.MediaAllowedHosts...)
	return clone
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	clone := make(map[string]string, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}

func (p *httpJSONProvider) ID() string { return p.config.ProviderID }

func (p *httpJSONProvider) Search(
	ctx context.Context,
	request ProviderSearchRequest,
) ([]ProviderCandidate, error) {
	if len([]rune(request.Query)) > p.config.MaxQueryRunes {
		return nil, fmt.Errorf("sticker search query exceeds %d characters", p.config.MaxQueryRunes)
	}
	if err := p.limiter.acquire(ctx); err != nil {
		return nil, err
	}
	httpRequest, secrets, err := p.buildRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	response, err := p.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// url.Error includes the request URL, which can contain query-string
		// credentials. Deliberately return a non-wrapping generic error.
		return nil, errors.New("sticker provider transport request failed")
	}
	defer response.Body.Close()
	if response.ContentLength > p.config.MaxResponseBytes {
		return nil, errors.New("sticker provider response exceeds configured size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, p.config.MaxResponseBytes+1))
	if err != nil {
		return nil, errors.New("read sticker provider response failed")
	}
	if int64(len(body)) > p.config.MaxResponseBytes {
		return nil, errors.New("sticker provider response exceeds configured size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPStatusError{
			ProviderID: p.ID(),
			StatusCode: response.StatusCode,
			Message:    redact(extractError(body, p.config.ErrorPath), secrets),
		}
	}
	return p.mapResponse(body, secrets)
}

func (p *httpJSONProvider) buildRequest(
	ctx context.Context,
	request ProviderSearchRequest,
) (*http.Request, []string, error) {
	values := templateValues{
		query:     request.Query,
		limit:     request.Limit,
		page:      request.Page,
		lookupEnv: p.lookupEnv,
	}
	endpoint, secrets, err := expandTemplate(p.config.Endpoint, values)
	if err != nil {
		return nil, nil, err
	}
	if err := validateProviderEndpoint(endpoint, p.config.AllowHTTPEndpoint); err != nil {
		return nil, nil, err
	}
	parsed, _ := url.Parse(endpoint)
	queryTemplates := p.config.Query
	formTemplates := p.config.Form
	if len(p.config.Parameters) > 0 {
		if p.config.Method == http.MethodGet {
			queryTemplates = p.config.Parameters
		} else {
			formTemplates = p.config.Parameters
		}
	}
	queryParameters, querySecrets, err := expandParameters(queryTemplates, values)
	if err != nil {
		return nil, nil, err
	}
	secrets = append(secrets, querySecrets...)
	formParameters, formSecrets, err := expandParameters(formTemplates, values)
	if err != nil {
		return nil, nil, err
	}
	secrets = append(secrets, formSecrets...)
	query := parsed.Query()
	for name, values := range queryParameters {
		query[name] = append([]string(nil), values...)
	}
	parsed.RawQuery = query.Encode()
	var body io.Reader
	if p.config.Method == http.MethodPost {
		body = strings.NewReader(formParameters.Encode())
	}
	httpRequest, err := http.NewRequestWithContext(ctx, p.config.Method, parsed.String(), body)
	if err != nil {
		return nil, nil, errors.New("create sticker provider request failed")
	}
	if p.config.Method == http.MethodPost {
		httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "golem-hermes-sticker/1.0")
	for name, template := range p.config.Headers {
		value, foundSecrets, expandErr := expandTemplate(template, values)
		if expandErr != nil {
			return nil, nil, expandErr
		}
		secrets = append(secrets, foundSecrets...)
		httpRequest.Header.Set(name, value)
	}
	return httpRequest, secrets, nil
}

func expandParameters(templates map[string]string, values templateValues) (url.Values, []string, error) {
	parameters := make(url.Values, len(templates))
	var secrets []string
	for name, template := range templates {
		value, foundSecrets, expandErr := expandTemplate(template, values)
		if expandErr != nil {
			return nil, nil, expandErr
		}
		secrets = append(secrets, foundSecrets...)
		parameters.Set(name, value)
	}
	return parameters, secrets, nil
}

func validateProviderEndpoint(rawURL string, allowHTTP bool) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || !parsed.IsAbs() {
		return errors.New("sticker provider endpoint must be an absolute URL without credentials")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return errors.New("sticker provider endpoint must use HTTPS")
	}
	return nil
}

func (p *httpJSONProvider) mapResponse(body []byte, secrets []string) ([]ProviderCandidate, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("sticker provider returned invalid JSON")
	}
	root := gjson.ParseBytes(body)
	if strings.TrimSpace(p.config.SuccessPath) != "" {
		status := resultAt(root, p.config.SuccessPath)
		code := comparableResult(status)
		success := false
		for _, expected := range p.config.SuccessValues {
			if code == strings.TrimSpace(expected) {
				success = true
				break
			}
		}
		if !success {
			message := ""
			if strings.TrimSpace(p.config.ErrorPath) != "" {
				message = comparableResult(resultAt(root, p.config.ErrorPath))
			}
			return nil, &BusinessError{
				ProviderID: p.ID(),
				Code:       redact(code, secrets),
				Message:    redact(message, secrets),
			}
		}
	}
	collection := resultAt(root, p.config.ItemsPath)
	if !collection.Exists() && collection.Raw == "" {
		return []ProviderCandidate{}, nil
	}
	items := []gjson.Result{collection}
	if collection.IsArray() {
		items = collection.Array()
	}
	result := make([]ProviderCandidate, 0, len(items))
	for _, item := range items {
		reference, err := applyTransforms(comparableResult(resultAt(item, p.config.URLPath)), p.config.URLTransforms)
		if err != nil {
			return nil, err
		}
		reference = strings.TrimSpace(reference)
		if reference == "" {
			continue
		}
		description := ""
		if strings.TrimSpace(p.config.DescriptionPath) != "" {
			description, err = applyTransforms(comparableResult(resultAt(item, p.config.DescriptionPath)), p.config.DescriptionTransforms)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, ProviderCandidate{
			Reference:   reference,
			Description: redact(description, secrets),
		})
	}
	return result, nil
}

func resultAt(value gjson.Result, path string) gjson.Result {
	path = strings.TrimSpace(path)
	if path == "" || path == "$" || path == "@this" {
		return value
	}
	path = strings.TrimPrefix(path, "$.")
	return value.Get(path)
}

func comparableResult(value gjson.Result) string {
	if value.Type == gjson.String {
		return value.String()
	}
	if value.Raw != "" {
		return strings.TrimSpace(value.Raw)
	}
	return strings.TrimSpace(value.String())
}

func extractError(body []byte, path string) string {
	if strings.TrimSpace(path) == "" || !gjson.ValidBytes(body) {
		return ""
	}
	return comparableResult(resultAt(gjson.ParseBytes(body), path))
}

func (p *httpJSONProvider) Materialize(
	ctx context.Context,
	candidate ProviderCandidate,
) (domain.EmojiOutput, error) {
	downloadContext, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	data, _, err := p.downloader.Download(downloadContext, candidate.Reference, DownloadPolicy{
		AllowedHosts: p.config.MediaAllowedHosts,
		MaxBytes:     p.config.MaxMediaBytes,
	})
	if err != nil {
		return domain.EmojiOutput{}, err
	}
	digest := md5.Sum(data)
	return domain.EmojiOutput{
		Data:        data,
		MD5:         hex.EncodeToString(digest[:]),
		Description: strings.TrimSpace(candidate.Description),
	}, nil
}
