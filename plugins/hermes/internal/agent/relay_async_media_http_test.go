package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func TestAsyncDeliveryHTTPDeliverV2CommitsStructuredOutputs(t *testing.T) {
	capability := &recordingAsyncCapability{result: domain.AsyncDeliveryResult{
		State: domain.AsyncDeliveryConsumed, Disposition: "delivered",
		MessageID: "message-1", OutboxIDs: []string{"outbox-1", "outbox-2"},
	}}
	gateway := newAsyncMediaGateway(t, capability, nil)
	gateway.config.AsyncDeliveryWake = func() { capability.wakeCount++ }
	server := httptest.NewServer(http.HandlerFunc(gateway.serveAsyncDeliveryDeliverV2))
	defer server.Close()

	status, body := postCapability(t, server.URL, asyncDeliverV2Request{
		Ticket:       "adt_1234567890123456789012345678901234567890",
		DelegationID: "deleg_v2", ProducerEpoch: "ade_epoch",
		HermesSessionID: "hermes-session", RelaySessionKey: "relay-session",
		ChatID: "chatroom:room|interactive", Profile: "default",
		Outputs: []domain.AsyncOutput{
			asyncHTTPOutput(t, "text", domain.TextOutput{Content: "hello"}),
			asyncHTTPOutput(t, "emoji", domain.EmojiOutput{Data: []byte("gif")}),
		},
	})
	if status != http.StatusOK || body["outbox_ids"] == nil || capability.wakeCount != 1 {
		t.Fatalf("deliver-v2 status=%d body=%#v wake=%d", status, body, capability.wakeCount)
	}
	if len(capability.committed.Outputs) != 2 || capability.committed.Content != "" {
		t.Fatalf("commit=%#v", capability.committed)
	}
}

func TestAsyncDeliveryHTTPDeliverV2RejectsInvalidOutput(t *testing.T) {
	capability := &recordingAsyncCapability{commitErr: storeport.ErrInvalid}
	gateway := newAsyncMediaGateway(t, capability, nil)
	server := httptest.NewServer(http.HandlerFunc(gateway.serveAsyncDeliveryDeliverV2))
	defer server.Close()

	status, _ := postCapability(t, server.URL, asyncDeliverV2Request{
		Ticket:       "adt_1234567890123456789012345678901234567890",
		DelegationID: "deleg_v2", ProducerEpoch: "ade_epoch",
		HermesSessionID: "hermes-session", RelaySessionKey: "relay-session",
		ChatID: "chatroom:room|interactive", Profile: "default",
		Outputs: []domain.AsyncOutput{
			asyncHTTPOutput(t, "video", map[string]string{"data": "inline"}),
		},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid output status=%d", status)
	}
}

func TestAsyncStickerHTTPUsesTicketBoundScope(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	stickers := &fakeStickerCapability{}
	capability := asyncStickerCapability(plainTicket)
	gateway := newAsyncMediaGateway(t, capability, stickers)
	server := newAsyncStickerServer(gateway)
	defer server.Close()

	bound := asyncBoundForTicket(plainTicket, capability.ticket)
	status, _ := postCapability(t, server.URL+asyncStickerSearchPath, asyncStickerSearchRequest{
		asyncBoundRequest: bound, Query: "ok", Limit: 1,
	})
	if status != http.StatusOK {
		t.Fatalf("search status=%d", status)
	}
	status, body := postCapability(t, server.URL+asyncStickerSelectPath, asyncStickerSelectRequest{
		asyncBoundRequest: bound, CandidateID: "candidate-1",
	})
	if status != http.StatusOK || body["data"] == "" {
		t.Fatalf("select status=%d body=%#v", status, body)
	}
	assertAsyncStickerScope(t, stickers, capability.ticket)
	bound.ChatID += "-wrong"
	status, _ = postCapability(t, server.URL+asyncStickerSearchPath, asyncStickerSearchRequest{
		asyncBoundRequest: bound, Query: "ok", Limit: 1,
	})
	if status != http.StatusConflict {
		t.Fatalf("wrong binding status=%d", status)
	}
}

func TestAsyncStickerSendQueuesBoundDirectOutput(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	stickers := &fakeStickerCapability{}
	capability := asyncStickerCapability(plainTicket)
	gateway := newAsyncMediaGateway(t, capability, stickers)
	gateway.config.AsyncDeliveryWake = func() { capability.wakeCount++ }
	server := newAsyncStickerServer(gateway)
	defer server.Close()

	bound := asyncBoundForTicket(plainTicket, capability.ticket)
	status, body := postCapability(
		t, server.URL+asyncStickerSendPath,
		asyncStickerSendRequest{
			asyncBoundRequest: bound, CandidateID: "candidate-1",
			InvocationID: "tool-call-1",
		},
	)
	if status != http.StatusOK || body["queued"] != true || capability.wakeCount != 1 {
		t.Fatalf("send status=%d body=%#v wake=%d", status, body, capability.wakeCount)
	}
	if capability.direct.InvocationID != "tool-call-1" ||
		capability.direct.Output.Kind != "emoji" ||
		capability.direct.ChatID != capability.ticket.ChatID {
		t.Fatalf("direct commit=%#v", capability.direct)
	}

	status, _ = postCapability(
		t, server.URL+asyncStickerSendPath,
		asyncStickerSendRequest{asyncBoundRequest: bound, CandidateID: "candidate-1"},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("missing invocation status=%d", status)
	}
}

func newAsyncMediaGateway(
	t *testing.T,
	capability *recordingAsyncCapability,
	stickers StickerCapability,
) *RelayGateway {
	t.Helper()
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: capability, Stickers: stickers,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	return gateway
}

func asyncStickerCapability(ticket string) *recordingAsyncCapability {
	return &recordingAsyncCapability{ticket: domain.AsyncDeliveryTicket{
		TicketHash: asyncTicketHash(ticket), Profile: "default",
		ProducerEpoch: "ade_epoch", DelegationID: "deleg_sticker",
		HermesSessionID: "hermes-session", RelaySessionKey: "relay-session",
		ChatID: "chatroom:room|interactive", SessionID: "chatroom:room",
		State: domain.AsyncDeliveryPending,
		Binding: domain.ChannelBinding{
			Principal: domain.Principal{ID: "wxid-owner"},
		},
	}}
}

func newAsyncStickerServer(gateway *RelayGateway) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(asyncStickerSearchPath, gateway.serveAsyncStickerSearch)
	mux.HandleFunc(asyncStickerSelectPath, gateway.serveAsyncStickerSelect)
	mux.HandleFunc(asyncStickerSendPath, gateway.serveAsyncStickerSend)
	return httptest.NewServer(mux)
}

func assertAsyncStickerScope(
	t *testing.T,
	stickers *fakeStickerCapability,
	ticket domain.AsyncDeliveryTicket,
) {
	t.Helper()
	if stickers.searchScope.RunID != "async:"+ticket.TicketHash ||
		stickers.selectScope.Principal.ID != "wxid-owner" {
		t.Fatalf("search=%#v select=%#v", stickers.searchScope, stickers.selectScope)
	}
}

func asyncHTTPOutput(t *testing.T, kind string, payload any) domain.AsyncOutput {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return domain.AsyncOutput{Kind: kind, Payload: data}
}

func asyncBoundForTicket(ticket string, value domain.AsyncDeliveryTicket) asyncBoundRequest {
	return asyncBoundRequest{
		Ticket: ticket, DelegationID: value.DelegationID,
		ProducerEpoch: value.ProducerEpoch, HermesSessionID: value.HermesSessionID,
		RelaySessionKey: value.RelaySessionKey, ChatID: value.ChatID,
		Profile: value.Profile,
	}
}
