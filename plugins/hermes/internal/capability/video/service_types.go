package video

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

var (
	ErrCandidateNotFound = errors.New("video candidate not found")
	ErrCandidateExpired  = errors.New("video candidate expired")
	ErrCandidateScope    = errors.New("video candidate belongs to a different run or chat")
	ErrRunVideoLimit     = errors.New("video limit for this run has been reached")
	ErrProviderNotFound  = errors.New("video provider not found")
)

type Scope struct {
	RunID  string `json:"run_id"`
	ChatID string `json:"chat_id"`
}

func (s Scope) validate() error {
	if strings.TrimSpace(s.RunID) == "" || strings.TrimSpace(s.ChatID) == "" {
		return errors.New("video scope requires run_id and chat_id")
	}
	return nil
}

type SearchRequest struct {
	Scope      Scope
	ProviderID string
	Query      string
	Category   string
	Limit      int
}

type Candidate struct {
	ID              string    `json:"id"`
	ProviderID      string    `json:"provider_id"`
	Title           string    `json:"title,omitempty"`
	PageURL         string    `json:"page_url,omitempty"`
	DurationSeconds uint32    `json:"duration_seconds,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type ProviderFailure struct {
	ProviderID string `json:"provider_id"`
	Message    string `json:"message"`
}

type SearchResult struct {
	Candidates []Candidate       `json:"candidates"`
	Failures   []ProviderFailure `json:"failures,omitempty"`
}

type URLRequest struct {
	Scope Scope
	URL   string
	Title string
}

type RegisteredProvider struct {
	Provider Provider
	Priority int
}

type ServiceConfig struct {
	DefaultCategory string
	CandidateTTL    time.Duration
	MaxRecords      int
	MaxResults      int
	MaxVideosPerRun int
	PrepareTimeout  time.Duration
	PrepareWorkers  int
}

type PreparationError struct {
	ProviderID  string
	FallbackURL string
	Cause       error
}

func (e *PreparationError) Error() string {
	if e == nil {
		return "video preparation failed"
	}
	message := fmt.Sprintf("video provider %q preparation failed", e.ProviderID)
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *PreparationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *PreparationError) Fallback() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.FallbackURL)
}

type SearchService interface {
	Search(context.Context, SearchRequest) (SearchResult, error)
	RegisterURL(context.Context, URLRequest) (Candidate, error)
	Select(context.Context, Scope, string) (domain.VideoOutput, error)
	Release(Scope)
}
