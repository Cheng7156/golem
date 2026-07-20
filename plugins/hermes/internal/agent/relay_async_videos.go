package agent

import (
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

type asyncVideoSendRequest struct {
	asyncBoundRequest
	CandidateID  string `json:"candidate_id"`
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
		writeCapabilityError(w, http.StatusBadGateway, err.Error())
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
