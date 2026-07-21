package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const (
	cronDeliveryRegisterPath = "/capabilities/v1/cron-delivery/register"
	cronDeliveryDeliverPath  = "/capabilities/v1/cron-delivery/deliver"
)

type CronDeliveryCapability interface {
	RegisterCronDelivery(
		context.Context,
		domain.CronDeliveryRegistration,
	) (domain.CronDeliveryBinding, error)
	CommitCronDelivery(
		context.Context,
		domain.CronDeliveryCommit,
	) (domain.CronDeliveryResult, error)
}

func (g *RelayGateway) registerCronDeliveryHandlers(mux *http.ServeMux) {
	mux.HandleFunc(cronDeliveryRegisterPath, g.serveCronDeliveryRegister)
	mux.HandleFunc(cronDeliveryDeliverPath, g.serveCronDeliveryDeliver)
	if _, ok := g.cronDirectCapability(); !ok {
		return
	}
	mux.HandleFunc(cronDirectStatusPath, g.serveCronDirectStatus)
	if g.config.Videos != nil {
		mux.HandleFunc(cronVideoSearchPath, g.serveCronVideoSearch)
		mux.HandleFunc(cronVideoSendPath, g.serveCronVideoSend)
		mux.HandleFunc(cronVideoStatusPath, g.serveCronVideoStatus)
	}
}

type cronRegisterRequest struct {
	JobID   string                   `json:"job_id"`
	Context capabilitySessionContext `json:"context"`
}

type cronDeliverRequest struct {
	Profile    string `json:"profile"`
	JobID      string `json:"job_id"`
	ChatID     string `json:"chat_id"`
	DeliveryID string `json:"delivery_id"`
	Content    string `json:"content"`
}

func (g *RelayGateway) serveCronDeliveryRegister(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input cronRegisterRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid cron delivery registration")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	registration, err := cronRegistration(input, run)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := g.config.CronDelivery.RegisterCronDelivery(request.Context(), registration); err != nil {
		writeAsyncStoreError(w, err, "could not register cron delivery")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{"registered": true})
}

func cronRegistration(
	input cronRegisterRequest,
	run *relayRun,
) (domain.CronDeliveryRegistration, error) {
	// Relay chat IDs may carry a session namespace (for example social-v2),
	// while durable Golem runs and cron bindings use the canonical session|lane
	// identity. capabilityRun already proved that the namespaced context belongs
	// to this run, so persist the canonical ID derived from the verified run.
	chatID := run.request.SessionID + "|" + string(run.request.Lane)
	value := domain.CronDeliveryRegistration{
		Profile: strings.TrimSpace(input.Context.Profile),
		JobID:   strings.TrimSpace(input.JobID), ChatID: chatID,
		ParentRunID: run.request.RunID,
	}
	return value, value.Validate()
}

func (g *RelayGateway) serveCronDeliveryDeliver(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input cronDeliverRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid cron delivery request")
		return
	}
	commit, err := cronCommit(input)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := g.config.CronDelivery.CommitCronDelivery(request.Context(), commit)
	if err != nil {
		writeAsyncStoreError(w, err, "could not commit cron delivery")
		return
	}
	if g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func cronCommit(input cronDeliverRequest) (domain.CronDeliveryCommit, error) {
	content := unwrapHermesPlainTextFallback(input.Content)
	if strings.Contains(content, "[TOOL_ERROR]") {
		return domain.CronDeliveryCommit{}, errors.New("cron delivery contains a tool error and cannot be sent")
	}
	visible, _, err := newRelayTextProposal(content)
	if err != nil {
		return domain.CronDeliveryCommit{}, err
	}
	value := domain.CronDeliveryCommit{
		Profile: strings.TrimSpace(input.Profile),
		JobID:   strings.TrimSpace(input.JobID), ChatID: strings.TrimSpace(input.ChatID),
		DeliveryID: strings.TrimSpace(input.DeliveryID), Content: visible,
	}
	return value, value.Validate()
}
