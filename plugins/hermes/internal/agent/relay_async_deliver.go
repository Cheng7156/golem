package agent

import (
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

func (g *RelayGateway) serveAsyncDeliveryDeliver(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncDeliverRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async delivery request")
		return
	}
	if err := validateAsyncTicket(input.Ticket); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	content := unwrapHermesPlainTextFallback(input.Content)
	visible, _, err := newRelayTextProposal(content)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	commit := asyncCommit(input, visible, g.isObserveResponse(content))
	result, err := g.config.AsyncDelivery.CommitAsyncDelivery(request.Context(), commit)
	if err != nil {
		writeAsyncStoreError(w, err, "could not commit async delivery")
		return
	}
	if result.OutboxID != "" && g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	g.releaseAsyncVideos(asyncTicketHash(input.Ticket), input.ChatID)
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveAsyncDeliveryDeliverV2(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncDeliverV2Request
	if decodeCapabilityRequestLimit(w, request, &input, maxAsyncDeliveryBody) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async delivery v2 request")
		return
	}
	if err := validateAsyncTicket(input.Ticket); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	commit := asyncCommitV2(input)
	result, err := g.config.AsyncDelivery.CommitAsyncDelivery(request.Context(), commit)
	if err != nil {
		writeAsyncStoreError(w, err, "could not commit async delivery")
		return
	}
	if len(result.OutboxIDs) > 0 && g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	g.releaseAsyncVideos(asyncTicketHash(input.Ticket), input.ChatID)
	writeCapabilityJSON(w, http.StatusOK, result)
}

func asyncCommit(input asyncDeliverRequest, content string, silent bool) domain.AsyncDeliveryCommit {
	return domain.AsyncDeliveryCommit{
		TicketHash: asyncTicketHash(input.Ticket), Profile: strings.TrimSpace(input.Profile),
		ProducerEpoch:   strings.TrimSpace(input.ProducerEpoch),
		DelegationID:    strings.TrimSpace(input.DelegationID),
		HermesSessionID: strings.TrimSpace(input.HermesSessionID),
		RelaySessionKey: strings.TrimSpace(input.RelaySessionKey),
		ChatID:          strings.TrimSpace(input.ChatID), Content: content, Silent: silent,
	}
}

func asyncCommitV2(input asyncDeliverV2Request) domain.AsyncDeliveryCommit {
	return domain.AsyncDeliveryCommit{
		TicketHash: asyncTicketHash(input.Ticket), Profile: strings.TrimSpace(input.Profile),
		ProducerEpoch:   strings.TrimSpace(input.ProducerEpoch),
		DelegationID:    strings.TrimSpace(input.DelegationID),
		HermesSessionID: strings.TrimSpace(input.HermesSessionID),
		RelaySessionKey: strings.TrimSpace(input.RelaySessionKey),
		ChatID:          strings.TrimSpace(input.ChatID),
		Outputs:         cloneAsyncRequestOutputs(input.Outputs), Silent: input.Silent,
	}
}

func cloneAsyncRequestOutputs(outputs []domain.AsyncOutput) []domain.AsyncOutput {
	values := make([]domain.AsyncOutput, 0, len(outputs))
	for _, output := range outputs {
		output.Payload = append([]byte(nil), output.Payload...)
		values = append(values, output)
	}
	return values
}
