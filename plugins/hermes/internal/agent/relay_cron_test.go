package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golem_plugin_hermes/internal/domain"

	"github.com/coder/websocket"
)

type recordingCronCapability struct {
	registered domain.CronDeliveryRegistration
	committed  domain.CronDeliveryCommit
	wakeCount  int
}

func (c *recordingCronCapability) RegisterCronDelivery(
	_ context.Context,
	registration domain.CronDeliveryRegistration,
) (domain.CronDeliveryBinding, error) {
	c.registered = registration
	return domain.CronDeliveryBinding{ID: "cron-binding-1"}, nil
}

func (c *recordingCronCapability) CommitCronDelivery(
	_ context.Context,
	commit domain.CronDeliveryCommit,
) (domain.CronDeliveryResult, error) {
	c.committed = commit
	return domain.CronDeliveryResult{
		Disposition: "delivered", MessageID: "cron-message-1", OutboxID: "cron-outbox-1",
	}, nil
}

func TestCronDeliveryHTTPRegisterBindsActiveRun(t *testing.T) {
	capability := &recordingCronCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, CronDelivery: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := stickerRunRequest("[direct]\nmessage: create cron")
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request), events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	server := httptest.NewServer(http.HandlerFunc(gateway.serveCronDeliveryRegister))
	defer server.Close()

	status, body := postCapability(t, server.URL, cronRegisterRequest{
		JobID: "job_gateway_status", Context: asyncHTTPContext(request),
	})
	if status != http.StatusOK || body["registered"] != true {
		t.Fatalf("register status=%d body=%#v", status, body)
	}
	if capability.registered.ParentRunID != request.RunID || capability.registered.ChatID != run.chatID {
		t.Fatalf("registration=%#v", capability.registered)
	}
}

func TestCronDeliveryHTTPRegisterAcceptsPerUserGroupSession(t *testing.T) {
	capability := &recordingCronCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, CronDelivery: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := stickerRunRequest("[group addressed]\nmessage: create cron")
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request), events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	server := httptest.NewServer(http.HandlerFunc(gateway.serveCronDeliveryRegister))
	defer server.Close()

	context := asyncHTTPContext(request)
	context.SessionKey += ":" + request.Principal.ID
	status, body := postCapability(t, server.URL, cronRegisterRequest{
		JobID: "job_group_video", Context: context,
	})
	if status != http.StatusOK || body["registered"] != true {
		t.Fatalf("register status=%d body=%#v", status, body)
	}
	if capability.registered.ChatID != run.chatID {
		t.Fatalf("registration=%#v", capability.registered)
	}
}

func TestCronDeliveryHTTPRegisterRejectsDifferentGroupParticipantSession(t *testing.T) {
	capability := &recordingCronCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, CronDelivery: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := stickerRunRequest("[group addressed]\nmessage: create cron")
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request), events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	server := httptest.NewServer(http.HandlerFunc(gateway.serveCronDeliveryRegister))
	defer server.Close()

	context := asyncHTTPContext(request)
	context.SessionKey += ":wxid-someone-else"
	status, body := postCapability(t, server.URL, cronRegisterRequest{
		JobID: "job_group_video", Context: context,
	})
	if status != http.StatusConflict || body["error"] != "session context does not match the active run" {
		t.Fatalf("register status=%d body=%#v", status, body)
	}
	if capability.registered.JobID != "" {
		t.Fatalf("unexpected registration=%#v", capability.registered)
	}
}

func TestCronDeliveryHTTPCommitUsesDurableCapability(t *testing.T) {
	capability := &recordingCronCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, CronDelivery: capability,
		AsyncDeliveryWake: func() { capability.wakeCount++ },
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveCronDeliveryDeliver))
	defer server.Close()
	status, body := postCapability(t, server.URL, cronDeliverRequest{
		Profile: "default", JobID: "job_gateway_status",
		ChatID:     "chatroom:room|interactive",
		DeliveryID: "job_gateway_status:2026-07-16T12:00:00+08:00",
		Content:    "**gateway ok**",
	})
	if status != http.StatusOK || body["message_id"] != "cron-message-1" {
		t.Fatalf("status=%d body=%#v", status, body)
	}
	if capability.committed.Content != "gateway ok" || capability.wakeCount != 1 {
		t.Fatalf("commit=%#v wake=%d", capability.committed, capability.wakeCount)
	}
}

func TestCronDeliveryHTTPRequiresCapabilityToken(t *testing.T) {
	capability := &recordingCronCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, CronDelivery: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveCronDeliveryDeliver))
	defer server.Close()
	status, _ := postCapabilityWithToken(t, capabilityPost{
		target: server.URL, token: "wrong-token",
		value: cronDeliverRequest{
			Profile: "default", JobID: "job_gateway_status",
			ChatID: "chatroom:room|interactive", DeliveryID: "fire-1", Content: "ok",
		},
	})
	if status != http.StatusUnauthorized || capability.committed.JobID != "" {
		t.Fatalf("status=%d commit=%#v", status, capability.committed)
	}
}

func TestRelayStillRejectsUnboundOrphanSend(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveRelay))
	defer server.Close()
	connection := dialRelay(t, server.URL)
	defer connection.CloseNow()
	writeRelayFrame(t, connection, map[string]any{"type": "hello"})
	readRelayFrame(t, connection)
	writeRelayFrame(t, connection, map[string]any{
		"type": "outbound", "requestId": "orphan-request",
		"action": map[string]any{
			"op": "send", "chat_id": "chatroom:room|interactive", "content": "forged",
			"metadata": map[string]any{"job_id": "forged"},
		},
	})
	result := readRelayFrame(t, connection)["result"].(map[string]any)
	if result["success"] != false || result["error"] != "no active run for chat" {
		t.Fatalf("result=%#v", result)
	}
}

func dialRelay(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	connection, _, err := websocket.Dial(
		context.Background(), "ws"+strings.TrimPrefix(serverURL, "http"), nil,
	)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return connection
}
