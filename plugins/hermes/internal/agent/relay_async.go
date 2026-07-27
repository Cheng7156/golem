package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const (
	asyncDeliveryRegisterPath  = "/capabilities/v1/async-delivery/register"
	asyncDeliveryStatusPath    = "/capabilities/v1/async-delivery/status"
	asyncDeliveryDeliverPath   = "/capabilities/v1/async-delivery/deliver"
	asyncDeliveryDeliverV2Path = "/capabilities/v1/async-delivery/deliver-v2"
	asyncStickerSearchPath     = "/capabilities/v1/async-delivery/stickers/search"
	asyncStickerSelectPath     = "/capabilities/v1/async-delivery/stickers/select"
	asyncStickerSendPath       = "/capabilities/v1/async-delivery/stickers/send"
	asyncVideoSearchPath       = "/capabilities/v1/async-delivery/videos/search"
	asyncVideoInspectPath      = "/capabilities/v1/async-delivery/videos/inspect"
	asyncVideoSendPath         = "/capabilities/v1/async-delivery/videos/send"
	asyncVideoSendURLPath      = "/capabilities/v1/async-delivery/videos/send-url"
	asyncVideoStatusPath       = "/capabilities/v1/async-delivery/videos/status"
	asyncDeliveryRevokePath    = "/capabilities/v1/async-delivery/revoke"
	asyncDeliveryReconcilePath = "/capabilities/v1/async-delivery/reconcile"
	maxAsyncDeliveryBody       = 12 << 20
)

type AsyncDeliveryCapability interface {
	RegisterAsyncDelivery(context.Context, domain.AsyncDeliveryRegistration) (domain.AsyncDeliveryTicket, error)
	GetAsyncDelivery(context.Context, string) (domain.AsyncDeliveryTicket, error)
	CommitAsyncDelivery(context.Context, domain.AsyncDeliveryCommit) (domain.AsyncDeliveryResult, error)
	CommitAsyncDirectOutput(context.Context, domain.AsyncDirectOutputCommit) (domain.AsyncDirectOutputResult, error)
	CountAsyncDirectOutputs(context.Context, string) (int64, error)
	RevokeAsyncDeliveries(context.Context, string, string, string) (int64, error)
	ReconcileAsyncDeliveries(context.Context, string, string) (int64, error)
}

type asyncRegisterRequest struct {
	Ticket          string                   `json:"ticket"`
	DelegationID    string                   `json:"delegation_id"`
	ProducerEpoch   string                   `json:"producer_epoch"`
	HermesSessionID string                   `json:"hermes_session_id"`
	Context         capabilitySessionContext `json:"context"`
}

type asyncBoundRequest struct {
	Ticket          string `json:"ticket"`
	DelegationID    string `json:"delegation_id"`
	ProducerEpoch   string `json:"producer_epoch"`
	HermesSessionID string `json:"hermes_session_id"`
	RelaySessionKey string `json:"relay_session_key"`
	ChatID          string `json:"chat_id"`
	Profile         string `json:"profile"`
}

type asyncDeliverRequest struct {
	Ticket          string `json:"ticket"`
	DelegationID    string `json:"delegation_id"`
	ProducerEpoch   string `json:"producer_epoch"`
	HermesSessionID string `json:"hermes_session_id"`
	RelaySessionKey string `json:"relay_session_key"`
	ChatID          string `json:"chat_id"`
	Profile         string `json:"profile"`
	Content         string `json:"content"`
}

type asyncDeliverV2Request struct {
	Ticket          string               `json:"ticket"`
	DelegationID    string               `json:"delegation_id"`
	ProducerEpoch   string               `json:"producer_epoch"`
	HermesSessionID string               `json:"hermes_session_id"`
	RelaySessionKey string               `json:"relay_session_key"`
	ChatID          string               `json:"chat_id"`
	Profile         string               `json:"profile"`
	Outputs         []domain.AsyncOutput `json:"outputs"`
	Silent          bool                 `json:"silent,omitempty"`
}

type asyncRevokeRequest struct {
	ProducerEpoch   string `json:"producer_epoch"`
	HermesSessionID string `json:"hermes_session_id"`
	Profile         string `json:"profile"`
}

type asyncReconcileRequest struct {
	ProducerEpoch string `json:"producer_epoch"`
	Profile       string `json:"profile"`
}

func capabilityPath(path string) bool {
	switch path {
	case stickerSearchPath, stickerMaterializePath, stickerSelectPath, stickerSelectManyPath,
		imageSearchPath, imageReadPath, stickerLibraryInventoryPath, stickerLibrarySearchPath,
		stickerLibraryPreviewPath, stickerLibraryPickPath, stickerLibraryCollectPath,
		stickerLibraryManagePath, silenceRuleAddPath,
		videoSearchPath, videoResolvePath, videoSelectPath, videoStatusPath,
		inlineVideoFetchPath,
		asyncDeliveryRegisterPath, asyncDeliveryStatusPath,
		asyncDeliveryDeliverPath, asyncDeliveryDeliverV2Path,
		asyncStickerSearchPath, asyncStickerSelectPath, asyncStickerSendPath,
		asyncVideoSearchPath, asyncVideoInspectPath, asyncVideoSendPath,
		asyncVideoSendURLPath, asyncVideoStatusPath,
		asyncDeliveryRevokePath, asyncDeliveryReconcilePath,
		cronDeliveryRegisterPath, cronDeliveryDeliverPath, cronDirectStatusPath,
		cronVideoSearchPath, cronVideoSendPath, cronVideoStatusPath:
		return true
	default:
		return false
	}
}

func (g *RelayGateway) serveAsyncDeliveryRegister(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncRegisterRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async delivery registration")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	registration, err := asyncRegistration(input, run)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	ticket, err := g.config.AsyncDelivery.RegisterAsyncDelivery(request.Context(), registration)
	if err != nil {
		writeAsyncStoreError(w, err, "could not register async delivery")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{"state": ticket.State})
}

func asyncRegistration(
	input asyncRegisterRequest,
	run *relayRun,
) (domain.AsyncDeliveryRegistration, error) {
	if err := validateAsyncTicket(input.Ticket); err != nil {
		return domain.AsyncDeliveryRegistration{}, err
	}
	value := domain.AsyncDeliveryRegistration{
		TicketHash:      asyncTicketHash(input.Ticket),
		Profile:         strings.TrimSpace(input.Context.Profile),
		ProducerEpoch:   strings.TrimSpace(input.ProducerEpoch),
		DelegationID:    strings.TrimSpace(input.DelegationID),
		HermesSessionID: strings.TrimSpace(input.HermesSessionID),
		RelaySessionKey: strings.TrimSpace(input.Context.SessionKey),
		ChatID:          strings.TrimSpace(input.Context.ChatID),
		ParentRunID:     run.request.RunID,
	}
	return value, value.Validate()
}

func (g *RelayGateway) serveAsyncDeliveryStatus(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncBoundRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async delivery status request")
		return
	}
	if err := validateAsyncTicket(input.Ticket); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	ticket, err := g.config.AsyncDelivery.GetAsyncDelivery(request.Context(), asyncTicketHash(input.Ticket))
	if err != nil {
		writeAsyncStoreError(w, err, "async delivery ticket was not found")
		return
	}
	if !asyncRequestMatches(ticket, input) {
		writeCapabilityError(w, http.StatusConflict, "async delivery binding does not match")
		return
	}
	count, err := g.config.AsyncDelivery.CountAsyncDirectOutputs(
		request.Context(), ticket.TicketHash,
	)
	if err != nil {
		writeAsyncStoreError(w, err, "could not read async direct delivery status")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"state": ticket.State, "direct_output_count": count,
	})
}

func (g *RelayGateway) serveAsyncDeliveryRevoke(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncRevokeRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async delivery revocation")
		return
	}
	if strings.TrimSpace(input.Profile) == "" ||
		strings.TrimSpace(input.ProducerEpoch) == "" ||
		strings.TrimSpace(input.HermesSessionID) == "" {
		writeCapabilityError(w, http.StatusBadRequest, "async delivery revocation has an empty binding")
		return
	}
	count, err := g.config.AsyncDelivery.RevokeAsyncDeliveries(
		request.Context(), strings.TrimSpace(input.Profile),
		strings.TrimSpace(input.ProducerEpoch), strings.TrimSpace(input.HermesSessionID),
	)
	if err != nil {
		writeAsyncStoreError(w, err, "could not revoke async deliveries")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{"revoked": count})
}

func (g *RelayGateway) serveAsyncDeliveryReconcile(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncReconcileRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async delivery reconciliation")
		return
	}
	if strings.TrimSpace(input.Profile) == "" || strings.TrimSpace(input.ProducerEpoch) == "" {
		writeCapabilityError(w, http.StatusBadRequest, "async delivery reconciliation has an empty binding")
		return
	}
	count, err := g.config.AsyncDelivery.ReconcileAsyncDeliveries(
		request.Context(), strings.TrimSpace(input.Profile),
		strings.TrimSpace(input.ProducerEpoch),
	)
	if err != nil {
		writeAsyncStoreError(w, err, "could not reconcile async deliveries")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{"abandoned": count})
}

func asyncRequestMatches(ticket domain.AsyncDeliveryTicket, input asyncBoundRequest) bool {
	return ticket.Profile == strings.TrimSpace(input.Profile) &&
		ticket.ProducerEpoch == strings.TrimSpace(input.ProducerEpoch) &&
		ticket.DelegationID == strings.TrimSpace(input.DelegationID) &&
		ticket.HermesSessionID == strings.TrimSpace(input.HermesSessionID) &&
		ticket.RelaySessionKey == strings.TrimSpace(input.RelaySessionKey) &&
		ticket.ChatID == strings.TrimSpace(input.ChatID)
}

func asyncTicketHash(ticket string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ticket)))
	return hex.EncodeToString(digest[:])
}

func validateAsyncTicket(ticket string) error {
	ticket = strings.TrimSpace(ticket)
	if len(ticket) < 36 || len(ticket) > 256 || !strings.HasPrefix(ticket, "adt_") {
		return errors.New("async delivery ticket is invalid")
	}
	return nil
}

func writeAsyncStoreError(w http.ResponseWriter, err error, message string) {
	if errors.Is(err, storeport.ErrInvalid) {
		writeCapabilityError(w, http.StatusBadRequest, message)
		return
	}
	if errors.Is(err, storeport.ErrNotFound) {
		writeCapabilityError(w, http.StatusNotFound, message)
		return
	}
	if errors.Is(err, storeport.ErrConflict) {
		writeCapabilityError(w, http.StatusConflict, message)
		return
	}
	writeCapabilityError(w, http.StatusInternalServerError, message)
}
