package agent

import (
	"encoding/json"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

type asyncStickerSearchRequest struct {
	asyncBoundRequest
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type asyncStickerSelectRequest struct {
	asyncBoundRequest
	CandidateID string `json:"candidate_id"`
}

type asyncStickerSendRequest struct {
	asyncBoundRequest
	CandidateID  string `json:"candidate_id"`
	InvocationID string `json:"invocation_id"`
}

func (g *RelayGateway) serveAsyncStickerSearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncStickerSearchRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async sticker search request")
		return
	}
	ticket, ok := g.asyncStickerTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	query := strings.TrimSpace(input.Query)
	if query == "" {
		writeCapabilityError(w, http.StatusBadRequest, "sticker query is empty")
		return
	}
	result, err := g.config.Stickers.Search(request.Context(), asyncStickerScope(ticket), query, input.Limit)
	if err != nil {
		writeCapabilityError(w, http.StatusBadGateway, "sticker search is temporarily unavailable")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveAsyncStickerSelect(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncStickerSelectRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async sticker selection")
		return
	}
	ticket, ok := g.asyncStickerTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	candidateID := strings.TrimSpace(input.CandidateID)
	if candidateID == "" {
		writeCapabilityError(w, http.StatusBadRequest, "candidate_id is empty")
		return
	}
	output, err := g.config.Stickers.Select(request.Context(), asyncStickerScope(ticket), candidateID)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "sticker candidate is unavailable")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, output)
}

func (g *RelayGateway) serveAsyncStickerSend(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input asyncStickerSendRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid async sticker send request")
		return
	}
	ticket, ok := g.asyncStickerTicket(w, request, input.asyncBoundRequest)
	if !ok {
		return
	}
	selection := asyncStickerSelection{
		writer: w, request: request, ticket: ticket, candidateID: input.CandidateID,
	}
	output, ok := g.selectAsyncSticker(selection)
	if !ok {
		return
	}
	commit, err := asyncStickerDirectCommit(ticket, input, output)
	if err != nil {
		writeCapabilityError(w, http.StatusInternalServerError, "could not encode async sticker")
		return
	}
	if err := commit.Validate(); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := g.config.AsyncDelivery.CommitAsyncDirectOutput(request.Context(), commit)
	if err != nil {
		writeAsyncStoreError(w, err, "could not queue async sticker")
		return
	}
	if result.Queued && g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

type asyncStickerSelection struct {
	writer      http.ResponseWriter
	request     *http.Request
	ticket      domain.AsyncDeliveryTicket
	candidateID string
}

func (g *RelayGateway) selectAsyncSticker(input asyncStickerSelection) (domain.EmojiOutput, bool) {
	candidateID := strings.TrimSpace(input.candidateID)
	if candidateID == "" {
		writeCapabilityError(input.writer, http.StatusBadRequest, "candidate_id is empty")
		return domain.EmojiOutput{}, false
	}
	output, err := g.config.Stickers.Select(
		input.request.Context(), asyncStickerScope(input.ticket), candidateID,
	)
	if err != nil {
		writeCapabilityError(
			input.writer, http.StatusBadRequest, "sticker candidate is unavailable",
		)
		return domain.EmojiOutput{}, false
	}
	return output, true
}

func asyncStickerDirectCommit(
	ticket domain.AsyncDeliveryTicket,
	input asyncStickerSendRequest,
	output domain.EmojiOutput,
) (domain.AsyncDirectOutputCommit, error) {
	payload, err := json.Marshal(output)
	if err != nil {
		return domain.AsyncDirectOutputCommit{}, err
	}
	return domain.AsyncDirectOutputCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID,
		RelaySessionKey: ticket.RelaySessionKey, ChatID: ticket.ChatID,
		InvocationID: strings.TrimSpace(input.InvocationID),
		Output:       domain.AsyncOutput{Kind: "emoji", Payload: payload},
	}, nil
}

func (g *RelayGateway) asyncStickerTicket(
	w http.ResponseWriter,
	request *http.Request,
	input asyncBoundRequest,
) (domain.AsyncDeliveryTicket, bool) {
	if g.config.Stickers == nil {
		writeCapabilityError(w, http.StatusServiceUnavailable, "stickers are not enabled")
		return domain.AsyncDeliveryTicket{}, false
	}
	return g.asyncBoundTicket(w, request, input)
}

func (g *RelayGateway) asyncBoundTicket(
	w http.ResponseWriter,
	request *http.Request,
	input asyncBoundRequest,
) (domain.AsyncDeliveryTicket, bool) {
	if err := validateAsyncTicket(input.Ticket); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return domain.AsyncDeliveryTicket{}, false
	}
	ticket, err := g.config.AsyncDelivery.GetAsyncDelivery(request.Context(), asyncTicketHash(input.Ticket))
	if err != nil {
		writeAsyncStoreError(w, err, "async delivery ticket was not found")
		return domain.AsyncDeliveryTicket{}, false
	}
	if ticket.State != domain.AsyncDeliveryPending || !asyncRequestMatches(ticket, input) {
		writeCapabilityError(w, http.StatusConflict, "async delivery binding does not match")
		return domain.AsyncDeliveryTicket{}, false
	}
	return ticket, true
}

func asyncStickerScope(ticket domain.AsyncDeliveryTicket) StickerScope {
	return StickerScope{
		RunID:     "async:" + ticket.TicketHash,
		SessionID: ticket.SessionID,
		ChatID:    ticket.ChatID,
		Principal: ticket.Binding.Principal,
	}
}
