package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"golem_plugin_hermes/internal/domain"
)

type recordingAsyncCapability struct {
	registered  domain.AsyncDeliveryRegistration
	committed   domain.AsyncDeliveryCommit
	direct      domain.AsyncDirectOutputCommit
	ticket      domain.AsyncDeliveryTicket
	revoked     []string
	reconciled  []string
	wakeCount   int
	result      domain.AsyncDeliveryResult
	commitErr   error
	directCount int64
}

func (f *recordingAsyncCapability) CommitAsyncDirectOutput(
	_ context.Context,
	value domain.AsyncDirectOutputCommit,
) (domain.AsyncDirectOutputResult, error) {
	f.direct = value
	f.directCount++
	return domain.AsyncDirectOutputResult{
		Queued: true, OutboxID: "outbox-direct", Sequence: 7,
		DirectOutputCount: f.directCount,
	}, nil
}

func (f *recordingAsyncCapability) CountAsyncDirectOutputs(
	context.Context, string,
) (int64, error) {
	return f.directCount, nil
}

func asyncHTTPContext(request RunRequest) capabilitySessionContext {
	chatID := relayChatID(request)
	return capabilitySessionContext{
		Platform: "relay", ChatID: chatID, UserID: request.Principal.ID,
		SessionKey: relaySessionKey(request, chatID), SessionID: request.SessionID,
		MessageID: request.MessageID, Profile: "default",
	}
}

func (f *recordingAsyncCapability) RegisterAsyncDelivery(
	_ context.Context,
	value domain.AsyncDeliveryRegistration,
) (domain.AsyncDeliveryTicket, error) {
	f.registered = value
	if f.ticket.TicketHash != "" {
		return f.ticket, nil
	}
	f.ticket = domain.AsyncDeliveryTicket{
		ID: "ticket-recorded", TicketHash: value.TicketHash, Profile: value.Profile,
		ProducerEpoch: value.ProducerEpoch, DelegationID: value.DelegationID,
		HermesSessionID: value.HermesSessionID, RelaySessionKey: value.RelaySessionKey,
		ChatID: value.ChatID, SessionID: "chatroom:room", ReceiverID: "room",
		ParentRunID: value.ParentRunID, State: domain.AsyncDeliveryPending,
		Binding: domain.ChannelBinding{Principal: domain.Principal{ID: "wxid-owner"}},
	}
	return f.ticket, nil
}

func (f *recordingAsyncCapability) GetAsyncDelivery(
	context.Context, string,
) (domain.AsyncDeliveryTicket, error) {
	return f.ticket, nil
}

func (f *recordingAsyncCapability) CommitAsyncDelivery(
	_ context.Context,
	value domain.AsyncDeliveryCommit,
) (domain.AsyncDeliveryResult, error) {
	f.committed = value
	if f.commitErr != nil {
		return domain.AsyncDeliveryResult{}, f.commitErr
	}
	if f.result.State != "" {
		return f.result, nil
	}
	return domain.AsyncDeliveryResult{
		State: domain.AsyncDeliveryConsumed, Disposition: "delivered",
		MessageID: "message-1", OutboxID: "outbox-1",
	}, nil
}

func (f *recordingAsyncCapability) RevokeAsyncDeliveries(
	_ context.Context, profile, epoch, sessionID string,
) (int64, error) {
	f.revoked = []string{profile, epoch, sessionID}
	return 2, nil
}

func (f *recordingAsyncCapability) ReconcileAsyncDeliveries(
	_ context.Context, profile, epoch string,
) (int64, error) {
	f.reconciled = []string{profile, epoch}
	return 3, nil
}

func newAsyncHTTPServer(t *testing.T, capability *recordingAsyncCapability) *httptest.Server {
	t.Helper()
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(asyncDeliveryStatusPath, gateway.serveAsyncDeliveryStatus)
	mux.HandleFunc(asyncDeliveryRevokePath, gateway.serveAsyncDeliveryRevoke)
	mux.HandleFunc(asyncDeliveryReconcilePath, gateway.serveAsyncDeliveryReconcile)
	return httptest.NewServer(mux)
}

type asyncControlFixture struct {
	capability *recordingAsyncCapability
	server     *httptest.Server
	bound      asyncBoundRequest
}

func newAsyncControlFixture(t *testing.T) asyncControlFixture {
	t.Helper()
	capability := &recordingAsyncCapability{ticket: domain.AsyncDeliveryTicket{
		Profile: "default", ProducerEpoch: "ade_epoch", DelegationID: "deleg_1234abcd",
		HermesSessionID: "hermes-session", RelaySessionKey: "relay-session",
		ChatID: "chatroom:room|interactive", State: domain.AsyncDeliveryPending,
	}}
	bound := asyncBoundRequest{
		Ticket:       "adt_1234567890123456789012345678901234567890",
		DelegationID: capability.ticket.DelegationID, ProducerEpoch: capability.ticket.ProducerEpoch,
		HermesSessionID: capability.ticket.HermesSessionID,
		RelaySessionKey: capability.ticket.RelaySessionKey,
		ChatID:          capability.ticket.ChatID, Profile: capability.ticket.Profile,
	}
	return asyncControlFixture{
		capability: capability, server: newAsyncHTTPServer(t, capability), bound: bound,
	}
}

func TestAsyncDeliveryHTTPRegisterAndDeliver(t *testing.T) {
	capability := &recordingAsyncCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken:   testCapabilityToken,
		AsyncDelivery:     capability,
		AsyncDeliveryWake: func() { capability.wakeCount++ },
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := stickerRunRequest("[direct]\nmessage: delegate")
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request),
		events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	mux := http.NewServeMux()
	mux.HandleFunc(asyncDeliveryRegisterPath, gateway.serveAsyncDeliveryRegister)
	mux.HandleFunc(asyncDeliveryDeliverPath, gateway.serveAsyncDeliveryDeliver)
	server := httptest.NewServer(mux)
	defer server.Close()

	ticket := "adt_1234567890123456789012345678901234567890"
	status, body := postCapability(t, server.URL+asyncDeliveryRegisterPath, asyncRegisterRequest{
		Ticket: ticket, DelegationID: "deleg_1234abcd", ProducerEpoch: "ade_epoch",
		HermesSessionID: "hermes-session", Context: asyncHTTPContext(request),
	})
	if status != http.StatusOK || body["state"] != string(domain.AsyncDeliveryPending) {
		t.Fatalf("register status=%d body=%#v", status, body)
	}
	if capability.registered.ParentRunID != request.RunID || capability.registered.TicketHash == ticket {
		t.Fatalf("registration=%#v", capability.registered)
	}

	status, body = postCapability(t, server.URL+asyncDeliveryDeliverPath, asyncDeliverRequest{
		Ticket: ticket, DelegationID: "deleg_1234abcd", ProducerEpoch: "ade_epoch",
		HermesSessionID: "hermes-session", RelaySessionKey: relaySessionKey(request, run.chatID),
		ChatID: run.chatID, Profile: "default", Content: "**result**",
	})
	if status != http.StatusOK || body["outbox_id"] != "outbox-1" || capability.wakeCount != 1 {
		t.Fatalf("deliver status=%d body=%#v wake=%d", status, body, capability.wakeCount)
	}
	if capability.committed.Content != "result" || capability.committed.TicketHash != capability.registered.TicketHash {
		t.Fatalf("commit=%#v", capability.committed)
	}
}

func TestAsyncDeliveryHTTPSilenceDoesNotWakeOutbox(t *testing.T) {
	capability := &recordingAsyncCapability{result: domain.AsyncDeliveryResult{
		State: domain.AsyncDeliveryConsumed, Disposition: "silent", MessageID: "message-1",
	}}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: capability,
		AsyncDeliveryWake: func() { capability.wakeCount++ },
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(asyncDeliveryDeliverPath, gateway.serveAsyncDeliveryDeliver)
	server := httptest.NewServer(mux)
	defer server.Close()

	status, body := postCapability(t, server.URL+asyncDeliveryDeliverPath, asyncDeliverRequest{
		Ticket:       "adt_1234567890123456789012345678901234567890",
		DelegationID: "deleg_silent1", ProducerEpoch: "ade_epoch",
		HermesSessionID: "hermes-session", RelaySessionKey: "relay-session",
		ChatID: "chatroom:room|interactive", Profile: "default", Content: "silent",
	})
	if status != http.StatusOK || body["disposition"] != "silent" {
		t.Fatalf("deliver status=%d body=%#v", status, body)
	}
	if !capability.committed.Silent || capability.wakeCount != 0 {
		t.Fatalf("commit=%#v wake=%d", capability.committed, capability.wakeCount)
	}
}

func TestAsyncDeliveryHTTPStatusAuthAndBinding(t *testing.T) {
	fixture := newAsyncControlFixture(t)
	defer fixture.server.Close()
	fixture.capability.directCount = 2
	status, _ := postCapabilityWithToken(t, capabilityPost{
		target: fixture.server.URL + asyncDeliveryStatusPath,
		token:  "wrong-token", value: fixture.bound,
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", status)
	}
	status, body := postCapability(
		t, fixture.server.URL+asyncDeliveryStatusPath, fixture.bound,
	)
	if status != http.StatusOK || body["state"] != string(domain.AsyncDeliveryPending) ||
		body["direct_output_count"] != float64(2) {
		t.Fatalf("status=%d body=%#v", status, body)
	}
	fixture.bound.ChatID += "-wrong"
	status, _ = postCapability(t, fixture.server.URL+asyncDeliveryStatusPath, fixture.bound)
	if status != http.StatusConflict {
		t.Fatalf("binding mismatch status=%d", status)
	}
}

func TestAsyncDeliveryHTTPRevokeAndReconcile(t *testing.T) {
	fixture := newAsyncControlFixture(t)
	defer fixture.server.Close()
	revoke := asyncRevokeRequest{
		Profile: "default", ProducerEpoch: "ade_epoch", HermesSessionID: "hermes-session",
	}
	status, body := postCapability(t, fixture.server.URL+asyncDeliveryRevokePath, revoke)
	if status != http.StatusOK || body["revoked"] != float64(2) {
		t.Fatalf("revoke status=%d body=%#v", status, body)
	}
	if len(fixture.capability.revoked) != 3 || fixture.capability.revoked[2] != "hermes-session" {
		t.Fatalf("revoke binding=%#v", fixture.capability.revoked)
	}

	reconcile := asyncReconcileRequest{Profile: "default", ProducerEpoch: "ade_epoch"}
	status, body = postCapability(t, fixture.server.URL+asyncDeliveryReconcilePath, reconcile)
	if status != http.StatusOK || body["abandoned"] != float64(3) {
		t.Fatalf("reconcile status=%d body=%#v", status, body)
	}
	if len(fixture.capability.reconciled) != 2 || fixture.capability.reconciled[1] != "ade_epoch" {
		t.Fatalf("reconcile binding=%#v", fixture.capability.reconciled)
	}
	status, _ = postCapability(
		t, fixture.server.URL+asyncDeliveryReconcilePath, asyncReconcileRequest{},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("empty reconciliation status=%d", status)
	}
}
