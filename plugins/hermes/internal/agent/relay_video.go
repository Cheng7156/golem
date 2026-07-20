package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const (
	videoSearchPath  = "/capabilities/v1/videos/search"
	videoResolvePath = "/capabilities/v1/videos/resolve"
	videoSelectPath  = "/capabilities/v1/videos/select"
	videoStatusPath  = "/capabilities/v1/videos/status"
)

type VideoScope struct {
	RunID     string
	SessionID string
	ChatID    string
	Principal domain.Principal
}

type VideoSearchInput struct {
	Query      string
	Category   string
	ProviderID string
	Limit      int
}

type VideoURLInput struct {
	URL   string
	Title string
}

type VideoCandidate struct {
	ID              string `json:"id"`
	ProviderID      string `json:"provider_id"`
	Title           string `json:"title,omitempty"`
	PageURL         string `json:"page_url,omitempty"`
	DurationSeconds uint32 `json:"duration_seconds,omitempty"`
}

type VideoProviderFailure struct {
	ProviderID string `json:"provider_id"`
	Message    string `json:"message"`
}

type VideoSearchResult struct {
	Candidates       []VideoCandidate       `json:"candidates"`
	Failures         []VideoProviderFailure `json:"failures,omitempty"`
	ExpiresInSeconds int                    `json:"expires_in"`
}

type VideoCapability interface {
	Search(context.Context, VideoScope, VideoSearchInput) (VideoSearchResult, error)
	ResolveURL(context.Context, VideoScope, VideoURLInput) (VideoCandidate, error)
	Select(context.Context, VideoScope, string) (domain.VideoOutput, error)
	Release(VideoScope)
}

type videoSearchRequest struct {
	Query      string                   `json:"query"`
	Category   string                   `json:"category"`
	ProviderID string                   `json:"provider_id,omitempty"`
	Limit      int                      `json:"limit"`
	Context    capabilitySessionContext `json:"context"`
}

type videoResolveRequest struct {
	URL     string                   `json:"url"`
	Title   string                   `json:"title,omitempty"`
	Context capabilitySessionContext `json:"context"`
}

type videoSelectRequest struct {
	CandidateID string                   `json:"candidate_id"`
	Context     capabilitySessionContext `json:"context"`
}

type videoStatusRequest struct {
	JobID   string                   `json:"job_id"`
	Context capabilitySessionContext `json:"context"`
}

func (g *RelayGateway) serveVideoSearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input videoSearchRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid video search request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	result, err := g.config.Videos.Search(request.Context(), videoScope(run), VideoSearchInput{
		Query: strings.TrimSpace(input.Query), Category: strings.TrimSpace(input.Category),
		ProviderID: strings.TrimSpace(input.ProviderID), Limit: input.Limit,
	})
	if err != nil {
		slog.Warn("[hermes] video search failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveVideoResolve(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input videoResolveRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid video URL request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	candidate, err := g.config.Videos.ResolveURL(request.Context(), videoScope(run), VideoURLInput{
		URL: strings.TrimSpace(input.URL), Title: strings.TrimSpace(input.Title),
	})
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{"candidate": candidate})
}

func (g *RelayGateway) serveVideoSelect(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input videoSelectRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid video selection request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	candidateID := strings.TrimSpace(input.CandidateID)
	if candidateID == "" {
		writeCapabilityError(w, http.StatusBadRequest, "candidate_id is empty")
		return
	}
	job, err := g.enqueueVideoJob(run, candidateID)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "state": job.State})
}

func (g *RelayGateway) serveVideoStatus(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input videoStatusRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid video status request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	job, err := g.videoJob(strings.TrimSpace(input.JobID), run.request.RunID)
	if err != nil {
		writeCapabilityError(w, http.StatusNotFound, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, job)
}

func videoScope(run *relayRun) VideoScope {
	return VideoScope{
		RunID: run.request.RunID, SessionID: run.request.SessionID, ChatID: run.chatID,
		Principal: run.request.Principal,
	}
}

func marshalVideoProposal(output domain.VideoOutput) (OutputProposal, error) {
	payload, err := json.Marshal(output)
	if err != nil {
		return OutputProposal{}, err
	}
	return OutputProposal{Kind: "video", Payload: payload}, nil
}
