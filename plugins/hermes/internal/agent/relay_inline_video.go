package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const inlineVideoFetchPath = "/capabilities/v1/videos/fetch"

type inlineVideoFetchRequest struct {
	URL             string                   `json:"url"`
	Title           string                   `json:"title,omitempty"`
	InvocationID    string                   `json:"invocation_id"`
	ProducerEpoch   string                   `json:"producer_epoch"`
	HermesSessionID string                   `json:"hermes_session_id"`
	Context         capabilitySessionContext `json:"context"`
}

func (g *RelayGateway) serveInlineVideoFetch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	if g.config.Videos == nil || g.config.AsyncDelivery == nil || g.config.AsyncVideoJobs == nil {
		writeCapabilityError(w, http.StatusServiceUnavailable, "durable video fetch is unavailable")
		return
	}
	var input inlineVideoFetchRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid inline video fetch request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	if !authorizeInteractiveMedia(w, run) {
		return
	}
	input.URL = strings.TrimSpace(input.URL)
	input.Title = strings.TrimSpace(input.Title)
	input.InvocationID = strings.TrimSpace(input.InvocationID)
	input.ProducerEpoch = strings.TrimSpace(input.ProducerEpoch)
	input.HermesSessionID = strings.TrimSpace(input.HermesSessionID)
	if input.URL == "" || len(input.URL) > 4096 || len(input.Title) > 300 ||
		input.InvocationID == "" || len(input.InvocationID) > domain.MaxAsyncInvocationIDLength ||
		input.ProducerEpoch == "" || input.HermesSessionID == "" ||
		strings.TrimSpace(input.Context.Profile) == "" {
		writeCapabilityError(w, http.StatusBadRequest, "inline video fetch binding is invalid")
		return
	}
	registration := inlineVideoRegistration(input, run)
	if err := registration.Validate(); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	ticket, err := g.config.AsyncDelivery.RegisterAsyncDelivery(request.Context(), registration)
	if err != nil {
		writeAsyncStoreError(w, err, "could not register inline video delivery")
		return
	}
	job := newInlineVideoJob(ticket, input.URL, input.Title, input.InvocationID)
	ctx, err := g.asyncVideoContext()
	if err != nil {
		writeCapabilityError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	stored, created, err := g.config.AsyncVideoJobs.CreateAsyncVideoJob(ctx, job)
	if err != nil {
		writeAsyncStoreError(w, err, "could not create inline video job")
		return
	}
	if created || stored.State == domain.AsyncVideoJobPending {
		go g.runAsyncVideoJob(ctx, stored, ticket)
	}
	response := asyncVideoJobResponse(stored)
	response["accepted"] = true
	response["deduplicated"] = !created
	writeCapabilityJSON(w, http.StatusAccepted, response)
}

func inlineVideoRegistration(
	input inlineVideoFetchRequest,
	run *relayRun,
) domain.AsyncDeliveryRegistration {
	seed := strings.Join([]string{
		strings.TrimSpace(input.Context.Profile), input.ProducerEpoch,
		run.request.RunID, input.InvocationID,
	}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	return domain.AsyncDeliveryRegistration{
		TicketHash:      hex.EncodeToString(digest[:]),
		Profile:         strings.TrimSpace(input.Context.Profile),
		ProducerEpoch:   input.ProducerEpoch,
		DelegationID:    "inline_" + hex.EncodeToString(digest[:18]),
		HermesSessionID: input.HermesSessionID,
		RelaySessionKey: strings.TrimSpace(input.Context.SessionKey),
		ChatID:          strings.TrimSpace(input.Context.ChatID),
		ParentRunID:     run.request.RunID,
	}
}
