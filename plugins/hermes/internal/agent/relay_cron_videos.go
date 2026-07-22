package agent

import (
	"net/http"
	"strings"
)

const (
	cronVideoSearchPath = "/capabilities/v1/cron-delivery/videos/search"
	cronVideoSendPath   = "/capabilities/v1/cron-delivery/videos/send"
	cronVideoStatusPath = "/capabilities/v1/cron-delivery/videos/status"
)

type cronVideoSearchRequest struct {
	cronBoundRequest
	Query      string `json:"query"`
	Category   string `json:"category"`
	ProviderID string `json:"provider_id"`
	Limit      int    `json:"limit"`
}

type cronVideoSendRequest struct {
	cronBoundRequest
	CandidateID  string `json:"candidate_id"`
	InvocationID string `json:"invocation_id"`
}

type cronVideoStatusRequest struct {
	cronBoundRequest
	JobID string `json:"video_job_id"`
}

func (g *RelayGateway) serveCronVideoSearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input cronVideoSearchRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid cron video search request")
		return
	}
	run, ok := g.cronVideoRun(w, request, input.cronBoundRequest)
	if !ok {
		return
	}
	result, err := g.config.Videos.Search(request.Context(), cronVideoScope(run), VideoSearchInput{
		Query: strings.TrimSpace(input.Query), Category: strings.TrimSpace(input.Category),
		ProviderID: strings.TrimSpace(input.ProviderID), Limit: input.Limit,
	})
	if err != nil {
		writeCapabilityError(w, videoSearchErrorStatus(err), err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveCronVideoSend(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input cronVideoSendRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid cron video send request")
		return
	}
	run, ok := g.cronVideoRun(w, request, input.cronBoundRequest)
	if !ok {
		return
	}
	job, err := g.startCronVideoJob(run, input)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusAccepted, cronVideoJobResponse(job))
}

func (g *RelayGateway) serveCronVideoStatus(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input cronVideoStatusRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid cron video status request")
		return
	}
	run, ok := g.cronVideoRun(w, request, input.cronBoundRequest)
	if !ok {
		return
	}
	job, ok := g.cronVideoJob(strings.TrimSpace(input.JobID), cronVideoScopeKey(run))
	if !ok {
		writeCapabilityError(w, http.StatusNotFound, "cron video job was not found")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, cronVideoJobResponse(job))
}

func (g *RelayGateway) cronVideoRun(
	w http.ResponseWriter,
	request *http.Request,
	input cronBoundRequest,
) (cronDirectRun, bool) {
	if g.config.Videos == nil {
		writeCapabilityError(w, http.StatusServiceUnavailable, "videos are not enabled")
		return cronDirectRun{}, false
	}
	return g.cronDirectRun(w, request, input)
}

func cronVideoScope(run cronDirectRun) VideoScope {
	return VideoScope{
		RunID: cronVideoScopeKey(run), SessionID: run.binding.SessionID,
		ChatID: run.binding.ChatID, Principal: run.binding.Binding.Principal,
	}
}

func cronVideoScopeKey(run cronDirectRun) string {
	return "cron:" + run.binding.ID + ":" + run.deliveryID
}
