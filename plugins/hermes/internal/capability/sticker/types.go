// Package sticker provides provider-neutral sticker discovery and safe
// materialization.  The package deliberately does not know about Relay or a
// WeChat receiver: callers bind opaque candidates to the active Run and chat.
package sticker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

var (
	ErrCandidateNotFound     = errors.New("sticker candidate not found")
	ErrCandidateExpired      = errors.New("sticker candidate expired")
	ErrCandidateScope        = errors.New("sticker candidate belongs to a different run or chat")
	ErrMaterializedCacheFull = errors.New("sticker materialized cache is full")
	ErrProviderNotFound      = errors.New("sticker provider not found")
)

// Scope is the immutable authority boundary for a search and later selection.
// Receiver IDs intentionally do not appear here; delivery always uses the
// ChannelBinding already persisted by the caller.
type Scope struct {
	RunID  string `json:"run_id"`
	ChatID string `json:"chat_id"`
}

func (s Scope) validate() error {
	if strings.TrimSpace(s.RunID) == "" || strings.TrimSpace(s.ChatID) == "" {
		return errors.New("sticker scope requires run_id and chat_id")
	}
	return nil
}

type SearchRequest struct {
	Scope      Scope  `json:"scope"`
	ProviderID string `json:"provider_id,omitempty"`
	Query      string `json:"query"`
	Limit      int    `json:"limit,omitempty"`
	Page       int    `json:"page,omitempty"`
}

// Candidate is safe to return to an Agent. ID is random and opaque; the
// provider locator (normally a URL) remains private to SearchService.
type Candidate struct {
	ID          string    `json:"id"`
	ProviderID  string    `json:"provider_id"`
	Description string    `json:"description,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ProviderCandidate is provider-internal data. Reference is interpreted only
// by the Provider that created it and is never exposed through Candidate.
type ProviderCandidate struct {
	Reference   string          `json:"reference"`
	Description string          `json:"description,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

type ProviderSearchRequest struct {
	Query string
	Limit int
	Page  int
}

// Provider is the extension boundary for HTTP search, a local library, or a
// future generator. Materialize must return bytes, never send to WeChat.
type Provider interface {
	ID() string
	Search(context.Context, ProviderSearchRequest) ([]ProviderCandidate, error)
	Materialize(context.Context, ProviderCandidate) (domain.EmojiOutput, error)
}

type SearchService interface {
	Search(context.Context, SearchRequest) ([]Candidate, error)
	Bind(context.Context, Scope, string, []ProviderCandidate) ([]Candidate, error)
	Materialize(context.Context, Scope, string) (domain.EmojiOutput, error)
}

// BusinessError represents a syntactically valid HTTP response rejected by
// the remote API's business status field.
type BusinessError struct {
	ProviderID string
	Code       string
	Message    string
}

func (e *BusinessError) Error() string {
	if e == nil {
		return "sticker provider rejected request"
	}
	base := fmt.Sprintf("sticker provider %q rejected request", e.ProviderID)
	if e.Code != "" {
		base += " (code " + e.Code + ")"
	}
	if strings.TrimSpace(e.Message) != "" {
		base += ": " + strings.TrimSpace(e.Message)
	}
	return base
}

type HTTPStatusError struct {
	ProviderID string
	StatusCode int
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "sticker provider HTTP error"
	}
	message := fmt.Sprintf("sticker provider %q returned HTTP %d", e.ProviderID, e.StatusCode)
	if strings.TrimSpace(e.Message) != "" {
		message += ": " + strings.TrimSpace(e.Message)
	}
	return message
}
