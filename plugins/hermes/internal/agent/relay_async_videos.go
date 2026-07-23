package agent

import (
	"log/slog"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

type asyncVideoSearchRequest struct {
	asyncBoundRequest
	Query      string `json:"query"`
	Category   string `json:"category"`
	ProviderID string `json:"provider_id"`
	Limit      int    `json:"limit"`
}

type asyncVideoInspectRequest struct {
	asyncBoundRequest
	URL string `json:"url"`
}

type asyncVideoSendRequest struct {
	asyncBoundRequest
	CandidateID  string `json:"candidate_id"`
	InvocationID string `json:"invocation_id"`
}

type asyncVideoSendURLRequest struct {
	asyncBoundRequest
	URL          string `json:"url"`
	Title        string `json:"title,omitempty"`
	InvocationID string `json:"invocation_id"`
}

type asyncVideoStatusRequest struct {
	asyncBoundRequest
	JobID string `json:"job_id"`
}

func (g *RelayGateway) serveAsyncVideoSearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncVideoSearchRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async video search request")
		return
	}
	ticket, ok := g.asyncVideoTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	result, err := g.config.Videos.Search(request.Context(), asyncVideoScope(ticket), VideoSearchInput{
		Query: strings.TrimSpace(input.Query), Category: strings.TrimSpace(input.Category),
		ProviderID: strings.TrimSpace(input.ProviderID), Limit: input.Limit,
	})
	if err != nil {
		slog.Warn("[hermes] async video search failed",
			"delegation_id", ticket.DelegationID,
			"provider_id", strings.TrimSpace(input.ProviderID),
			"category", strings.TrimSpace(input.Category),
			"err", err,
		)
		writeCapabilityError(w, videoSearchErrorStatus(err), err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveAsyncVideoInspect(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncVideoInspectRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async video inspection request")
		return
	}
	ticket, ok := g.asyncVideoTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	rawURL := strings.TrimSpace(input.URL)
	if rawURL == "" || len(rawURL) > 4096 {
		writeCapabilityError(w, http.StatusBadRequest, "video URL is empty or too long")
		return
	}
	result, err := g.config.Videos.InspectURL(request.Context(), asyncVideoScope(ticket), rawURL)
	if err != nil {
		slog.Warn("[hermes] async video URL inspection failed",
			"delegation_id", ticket.DelegationID, "err", err)
		writeCapabilityError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := g.rememberAsyncVideoURLs(request.Context(), ticket.TicketHash, result); err != nil {
		slog.Warn("[hermes] could not persist async video URL inspection",
			"delegation_id", ticket.DelegationID, "err", err)
		writeCapabilityError(w, http.StatusInternalServerError, "could not persist video URL inspection")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveAsyncVideoSend(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncVideoSendRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async video send request")
		return
	}
	ticket, ok := g.asyncVideoTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	job, err := g.startAsyncVideoJob(ticket, input)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusAccepted, asyncVideoJobResponse(job))
}

func (g *RelayGateway) serveAsyncVideoSendURL(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncVideoSendURLRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async video URL send request")
		return
	}
	ticket, ok := g.asyncVideoTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	job, err := g.startAsyncVideoURLJob(ticket, input)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusAccepted, asyncVideoJobResponse(job))
}

func (g *RelayGateway) serveAsyncVideoStatus(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncVideoStatusRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async video status request")
		return
	}
	ticket, ok := g.asyncVideoTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	job, ok := g.asyncVideoJob(strings.TrimSpace(input.JobID), ticket.TicketHash)
	if !ok {
		writeCapabilityError(w, http.StatusNotFound, "async video job was not found")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, asyncVideoJobResponse(job))
}

func (g *RelayGateway) asyncVideoTicket(
	w http.ResponseWriter,
	request *http.Request,
	input asyncBoundRequest,
) (domain.AsyncDeliveryTicket, bool) {
	if g.config.Videos == nil {
		writeCapabilityError(w, http.StatusServiceUnavailable, "videos are not enabled")
		return domain.AsyncDeliveryTicket{}, false
	}
	return g.asyncBoundTicket(w, request, input)
}

func asyncVideoScope(ticket domain.AsyncDeliveryTicket) VideoScope {
	return VideoScope{
		RunID: "async:" + ticket.TicketHash, SessionID: ticket.SessionID,
		ChatID: ticket.ChatID, Principal: ticket.Binding.Principal,
	}
}
