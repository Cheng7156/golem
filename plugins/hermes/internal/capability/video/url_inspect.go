package video

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	maximumURLLength      = 4096
	maximumJSONDepth      = 8
	maximumJSONStringSize = 4096
	maximumURLCandidates  = 32
)

type URLInspectConfig struct {
	Timeout   time.Duration
	MaxBytes  int64
	AllowHTTP bool
	Workers   int
}

type URLReference struct {
	Path  string `json:"path"`
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
	Score int    `json:"score"`
}

type URLInspection struct {
	Kind        string         `json:"kind"`
	FinalURL    string         `json:"final_url,omitempty"`
	ContentType string         `json:"content_type,omitempty"`
	Document    any            `json:"document,omitempty"`
	Candidates  []URLReference `json:"candidates,omitempty"`
}

type URLInspector struct {
	client    *http.Client
	maxBytes  int64
	allowHTTP bool
	slots     chan struct{}
}

type URLInspectorOption func(*URLInspector)

func WithURLInspectorHTTPClient(client *http.Client) URLInspectorOption {
	return func(inspector *URLInspector) {
		if client != nil {
			inspector.client = client
		}
	}
}

func NewURLInspector(config URLInspectConfig, options ...URLInspectorOption) (*URLInspector, error) {
	if config.Timeout <= 0 || config.MaxBytes <= 0 {
		return nil, errors.New("URL inspector configuration is invalid")
	}
	if config.Workers <= 0 {
		config.Workers = 1
	}
	inspector := &URLInspector{
		client:   secureDownloadClient(DownloadConfig{Timeout: config.Timeout, AllowHTTP: config.AllowHTTP}),
		maxBytes: config.MaxBytes, allowHTTP: config.AllowHTTP, slots: make(chan struct{}, config.Workers),
	}
	for _, option := range options {
		option(inspector)
	}
	return inspector, nil
}

func (i *URLInspector) Inspect(ctx context.Context, rawURL string) (URLInspection, error) {
	select {
	case i.slots <- struct{}{}:
		defer func() { <-i.slots }()
	case <-ctx.Done():
		return URLInspection{}, ctx.Err()
	}
	parsed, err := validateInspectableURL(rawURL, i.allowHTTP)
	if err != nil {
		return URLInspection{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return URLInspection{}, fmt.Errorf("create URL inspection request: %w", err)
	}
	request.Header.Set("Accept", "video/*, application/json, text/plain;q=0.9")
	request.Header.Set("User-Agent", "golem-hermes-video-fetch/1.0")
	response, err := i.client.Do(request)
	if err != nil {
		return URLInspection{}, fmt.Errorf("inspect video URL: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return URLInspection{}, fmt.Errorf("video URL returned HTTP %d", response.StatusCode)
	}
	finalURL := ""
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	reader := bufio.NewReader(response.Body)
	peeked, _ := reader.Peek(512)
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	jsonLike := looksLikeJSON(contentType, peeked)
	if !jsonLike && (strings.HasPrefix(contentType, "video/") ||
		looksLikeVideo(peeked) || contentType == "application/octet-stream") {
		return URLInspection{Kind: "video", FinalURL: finalURL, ContentType: contentType}, nil
	}
	body, err := readBytesLimit(reader, i.maxBytes)
	if err != nil {
		return URLInspection{}, err
	}
	if looksLikeJSON(contentType, body) {
		return inspectJSON(finalURL, contentType, body, i.allowHTTP)
	}
	if mediaURL := textMediaURL(body, i.allowHTTP); mediaURL != "" {
		return URLInspection{Kind: "url", FinalURL: finalURL, ContentType: contentType,
			Candidates: []URLReference{{Path: "$", URL: mediaURL, Label: "text response", Score: 100}}}, nil
	}
	return URLInspection{}, fmt.Errorf("video URL returned unsupported content type %q", contentType)
}

func validateInspectableURL(raw string, allowHTTP bool) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maximumURLLength {
		return nil, errors.New("video URL is empty or too long")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return nil, errors.New("video URL must be absolute and contain no credentials")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return nil, errors.New("video URL must use HTTPS or explicitly allowed HTTP")
	}
	return parsed, nil
}

func inspectJSON(finalURL, contentType string, body []byte, allowHTTP bool) (URLInspection, error) {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return URLInspection{}, errors.New("video URL returned invalid JSON")
	}
	if text, ok := document.(string); ok {
		if mediaURL := textMediaURL([]byte(text), allowHTTP); mediaURL != "" {
			return URLInspection{Kind: "url", FinalURL: finalURL, ContentType: contentType,
				Candidates: []URLReference{{Path: "$", URL: mediaURL, Label: "JSON string", Score: 100}}}, nil
		}
	}
	values := make([]URLReference, 0, 8)
	base, _ := url.Parse(finalURL)
	collectJSONURLs(document, "$", "", 0, base, allowHTTP, &values)
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].Score != values[right].Score {
			return values[left].Score > values[right].Score
		}
		return values[left].Path < values[right].Path
	})
	if len(values) > maximumURLCandidates {
		values = values[:maximumURLCandidates]
	}
	sanitized := sanitizeJSON(document, "", 0)
	return URLInspection{Kind: "json", FinalURL: finalURL, ContentType: contentType,
		Document: sanitized, Candidates: values}, nil
}

func collectJSONURLs(value any, path, key string, depth int, base *url.URL, allowHTTP bool, result *[]URLReference) {
	if depth > maximumJSONDepth || len(*result) >= maximumURLCandidates*2 {
		return
	}
	switch item := value.(type) {
	case map[string]any:
		for name, child := range item {
			if isSensitiveJSONKey(name) {
				continue
			}
			childPath := path + "." + name
			collectJSONURLs(child, childPath, name, depth+1, base, allowHTTP, result)
		}
	case []any:
		for index, child := range item {
			collectJSONURLs(child, fmt.Sprintf("%s[%d]", path, index), key, depth+1, base, allowHTTP, result)
		}
	case string:
		candidate := strings.TrimSpace(item)
		parsed, err := inspectJSONURL(candidate, base, allowHTTP)
		if err != nil {
			return
		}
		label := strings.TrimSpace(key)
		score := urlFieldScore(label, parsed.Path)
		*result = append(*result, URLReference{Path: path, URL: parsed.String(), Label: label, Score: score})
	}
}

func inspectJSONURL(value string, base *url.URL, allowHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.User != nil {
		return nil, errors.New("invalid JSON URL")
	}
	if !parsed.IsAbs() {
		if base == nil || (parsed.Path != "" && !strings.HasPrefix(parsed.Path, "/") &&
			!strings.HasPrefix(parsed.Path, "./") && !strings.HasPrefix(parsed.Path, "../")) {
			return nil, errors.New("relative JSON value is not a URL")
		}
		parsed = base.ResolveReference(parsed)
	}
	return validateInspectableURL(parsed.String(), allowHTTP)
}

func urlFieldScore(key, path string) int {
	value := strings.ToLower(key + " " + path)
	score := 10
	for _, token := range []string{"video", "media", "play", "download", "source", "src", "stream", "file"} {
		if strings.Contains(value, token) {
			score += 20
		}
	}
	for _, suffix := range []string{".mp4", ".webm", ".mov", ".mkv", ".avi"} {
		if strings.Contains(strings.ToLower(path), suffix) {
			score += 15
		}
	}
	if strings.Contains(strings.ToLower(path), ".m3u8") {
		score -= 30
	}
	return score
}

func sanitizeJSON(value any, key string, depth int) any {
	if depth > maximumJSONDepth {
		return "[depth limited]"
	}
	if isSensitiveJSONKey(key) {
		return "[redacted]"
	}
	switch item := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(item))
		for name, child := range item {
			result[name] = sanitizeJSON(child, name, depth+1)
		}
		return result
	case []any:
		result := make([]any, len(item))
		for index, child := range item {
			result[index] = sanitizeJSON(child, key, depth+1)
		}
		return result
	case string:
		if len([]rune(item)) > maximumJSONStringSize {
			return string([]rune(item)[:maximumJSONStringSize]) + "..."
		}
		return item
	default:
		return item
	}
}

func isSensitiveJSONKey(key string) bool {
	value := strings.ToLower(strings.TrimSpace(key))
	for _, token := range []string{"token", "secret", "password", "passwd", "cookie", "authorization", "api_key", "apikey"} {
		if strings.Contains(value, token) {
			return true
		}
	}
	return false
}

func textMediaURL(body []byte, allowHTTP bool) string {
	value := strings.TrimSpace(string(bytes.TrimSpace(body)))
	if value == "" {
		return ""
	}
	if parsed, err := validateInspectableURL(value, allowHTTP); err == nil {
		return parsed.String()
	}
	for _, scheme := range []string{"https://", "http://"} {
		index := strings.Index(value, scheme)
		if index < 0 {
			continue
		}
		candidate := value[index:]
		if end := strings.IndexAny(candidate, " \t\r\n\"'<>),]"); end >= 0 {
			candidate = candidate[:end]
		}
		if parsed, err := validateInspectableURL(candidate, allowHTTP); err == nil {
			return parsed.String()
		}
	}
	return ""
}
