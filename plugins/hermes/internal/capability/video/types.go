// Package video provides provider-neutral video discovery and resolution.
// It does not persist media or send messages; callers own those boundaries.
package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrNoCandidates      = errors.New("video provider returned no candidates")
	ErrUnexpectedBinary  = errors.New("video provider returned binary media during discovery")
	ErrInvalidReference  = errors.New("invalid video provider candidate reference")
	ErrUnsupportedFormat = errors.New("unsupported video provider response format")
)

type DiscoveryRequest struct {
	Query    string
	Category string
	Limit    int
	Page     int
}

type ProviderCandidate struct {
	Reference       string
	Title           string
	PageURL         string
	DurationSeconds uint32
}

type MediaSource struct {
	URL           string
	Headers       http.Header
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
}

func (s *MediaSource) Close() error {
	if s == nil || s.Body == nil {
		return nil
	}
	return s.Body.Close()
}

type ResolvedVideo struct {
	Title           string
	PageURL         string
	FallbackURL     string
	DurationSeconds uint32
	Source          MediaSource
}

type Provider interface {
	ID() string
	Categories() []string
	Discover(context.Context, DiscoveryRequest) ([]ProviderCandidate, error)
	Resolve(context.Context, ProviderCandidate) (ResolvedVideo, error)
	FallbackURL(ProviderCandidate) string
}

type FieldMapping struct {
	Path       string
	Transforms []string
}

type ResponseMapping struct {
	SuccessPath   string
	SuccessValues []string
	ItemsPath     string
	ErrorPath     string
	URL           FieldMapping
	Title         FieldMapping
	PageURL       FieldMapping
	Duration      FieldMapping
}

type HTTPProviderConfig struct {
	ProviderID          string
	Categories          []string
	Endpoint            string
	Method              string
	RequestMode         string
	ResponseMode        string
	MaterializationMode string
	Headers             map[string]string
	MediaHeaders        map[string]string
	Query               map[string]string
	Form                map[string]string
	JSONBody            map[string]any
	AllowedMediaHosts   []string
	PublicFallbackURL   string
	FallbackURLPolicy   string
	Response            ResponseMapping
	Timeout             time.Duration
	RequestsPerMinute   int
	MaxMetadataBytes    int64
	MaxSourceBytes      int64
	AllowHTTP           bool
}

type HTTPProviderOption func(*httpProvider)

func WithHTTPClient(client *http.Client) HTTPProviderOption {
	return func(provider *httpProvider) {
		if client != nil {
			provider.client = client
		}
	}
}

func WithEnvironment(lookup func(string) (string, bool)) HTTPProviderOption {
	return func(provider *httpProvider) {
		if lookup != nil {
			provider.lookupEnv = lookup
		}
	}
}

type BusinessError struct {
	ProviderID string
	Code       string
	Message    string
}

func (e *BusinessError) Error() string {
	if e == nil {
		return "video provider rejected request"
	}
	message := fmt.Sprintf("video provider %q rejected request", e.ProviderID)
	if e.Code != "" {
		message += " (code " + e.Code + ")"
	}
	if strings.TrimSpace(e.Message) != "" {
		message += ": " + strings.TrimSpace(e.Message)
	}
	return message
}

type HTTPStatusError struct {
	ProviderID string
	StatusCode int
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "video provider HTTP error"
	}
	message := fmt.Sprintf("video provider %q returned HTTP %d", e.ProviderID, e.StatusCode)
	if strings.TrimSpace(e.Message) != "" {
		message += ": " + strings.TrimSpace(e.Message)
	}
	return message
}
