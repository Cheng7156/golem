package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"golem_plugin_hermes/internal/domain"
)

const testCapabilityToken = "test-capability-token-32-bytes"

type fakeStickerCapability struct {
	mu               sync.Mutex
	searchScope      StickerScope
	selectScope      StickerScope
	materializeScope StickerScope
	query            string
	selectedID       string
}

func (f *fakeStickerCapability) Search(_ context.Context, scope StickerScope, query string, limit int) (StickerSearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchScope = scope
	f.query = query
	return StickerSearchResult{
		Candidates:       []StickerCandidate{{ID: "candidate-1", Description: "开心", Format: "png"}},
		ExpiresInSeconds: 300,
	}, nil
}

func (f *fakeStickerCapability) Select(_ context.Context, scope StickerScope, candidateID string) (domain.EmojiOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectScope = scope
	f.selectedID = candidateID
	return fakeStickerOutput(candidateID)
}

func (f *fakeStickerCapability) Materialize(_ context.Context, scope StickerScope, candidateID string) (domain.EmojiOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.materializeScope = scope
	return fakeStickerOutput(candidateID)
}

func fakeStickerOutput(candidateID string) (domain.EmojiOutput, error) {
	if candidateID != "candidate-1" {
		return domain.EmojiOutput{}, fmt.Errorf("%w: %s", ErrStickerCandidateUnavailable, candidateID)
	}
	return domain.EmojiOutput{
		Data: []byte("sticker-bytes"), MIMEType: "image/png", Description: "开心",
	}, nil
}

func TestStickerCapabilityRequiresAuthenticationAndExactRunContext(t *testing.T) {
	capability := &fakeStickerCapability{}
	gateway, err := NewRelayGateway(RelayConfig{CapabilityToken: testCapabilityToken, Stickers: capability})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := RunRequest{
		RunID: "run-scope", SessionID: "chatroom:room-1", Lane: domain.LaneInteractive,
		Principal: domain.Principal{ID: "wxid-owner"}, Input: "hello", ChatType: "group",
		MessageID: "event-scope", PlatformMessageID: "9001",
	}
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request), events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	server := httptest.NewServer(http.HandlerFunc(gateway.serveStickerSearch))
	defer server.Close()

	payload := stickerSearchRequest{
		Query: "开心", Limit: 3,
		Context: capabilityContext(request),
	}
	status, _ := postCapabilityWithoutAuth(t, server.URL, payload)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d, want %d", status, http.StatusUnauthorized)
	}

	missing := payload
	missing.Context.SessionKey = ""
	status, _ = postCapability(t, server.URL, missing)
	if status != http.StatusConflict {
		t.Fatalf("missing context status=%d, want %d", status, http.StatusConflict)
	}

	wrong := payload
	wrong.Context.SessionKey += "-other"
	status, _ = postCapability(t, server.URL, wrong)
	if status != http.StatusConflict {
		t.Fatalf("wrong session status=%d, want %d", status, http.StatusConflict)
	}

	stale := payload
	stale.Context.MessageID = "event-from-previous-turn"
	status, _ = postCapability(t, server.URL, stale)
	if status != http.StatusConflict {
		t.Fatalf("stale message status=%d, want %d", status, http.StatusConflict)
	}

	platform := payload
	platform.Context.MessageID = request.PlatformMessageID
	status, response := postCapability(t, server.URL, platform)
	if status != http.StatusOK {
		t.Fatalf("platform message status=%d response=%v", status, response)
	}

	status, response = postCapability(t, server.URL, payload)
	if status != http.StatusOK {
		t.Fatalf("valid search status=%d response=%v", status, response)
	}
	if candidates, ok := response["candidates"].([]any); !ok || len(candidates) != 1 {
		t.Fatalf("unexpected candidates: %#v", response["candidates"])
	}
	capability.mu.Lock()
	defer capability.mu.Unlock()
	if capability.searchScope.RunID != request.RunID || capability.searchScope.ChatID != run.chatID || capability.query != "开心" {
		t.Fatalf("search scope=%#v query=%q", capability.searchScope, capability.query)
	}
}

func TestAmbientRunRejectsStickerCapabilitiesBeforeProvider(t *testing.T) {
	capability := &fakeStickerCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, Stickers: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := RunRequest{
		RunID: "run-ambient-sticker", SessionID: "chatroom:room-ambient",
		Lane: domain.LaneInteractive, TriggerKind: domain.TriggerAmbient,
		Principal: domain.Principal{ID: "wxid-owner"}, Input: "group chatter",
		ChatType: "group", MessageID: "event-ambient-sticker",
	}
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request),
		events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	mux := http.NewServeMux()
	mux.HandleFunc(stickerSearchPath, gateway.serveStickerSearch)
	mux.HandleFunc(stickerMaterializePath, gateway.serveStickerMaterialize)
	mux.HandleFunc(stickerSelectPath, gateway.serveStickerSelect)
	server := httptest.NewServer(mux)
	defer server.Close()

	for _, test := range []struct {
		name string
		path string
		body any
	}{
		{name: "search", path: stickerSearchPath, body: stickerSearchRequest{
			Query: "开心", Limit: 1, Context: capabilityContext(request),
		}},
		{name: "materialize", path: stickerMaterializePath, body: stickerSelectRequest{
			CandidateID: "candidate-1", Context: capabilityContext(request),
		}},
		{name: "select", path: stickerSelectPath, body: stickerSelectRequest{
			CandidateID: "candidate-1", Context: capabilityContext(request),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, response := postCapability(t, server.URL+test.path, test.body)
			if status != http.StatusForbidden || response["error"] != ambientMediaDenied {
				t.Fatalf("status=%d response=%#v", status, response)
			}
		})
	}
	capability.mu.Lock()
	defer capability.mu.Unlock()
	if capability.query != "" || capability.selectedID != "" ||
		capability.materializeScope.RunID != "" {
		t.Fatalf("ambient request reached sticker provider: %#v", capability)
	}
}

func TestStickerCapabilityRejectsRelayPathCollision(t *testing.T) {
	for _, path := range []string{stickerSearchPath, stickerMaterializePath, stickerSelectPath} {
		if _, err := NewRelayGateway(RelayConfig{
			Path: path, CapabilityToken: testCapabilityToken, Stickers: &fakeStickerCapability{},
		}); err == nil {
			t.Fatalf("expected relay path %q collision to be rejected", path)
		}
	}
}

func TestStickerCapabilityCompletesStickerOnlyReply(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[group addressed]\nmessage: hello")
	defer fixture.close(t)

	fixture.selectSticker(t)
	fixture.sendFinal(t, "[[GOLEM_HERMES EFFECT_ONLY_V1]] ]")

	accepted := recvRelayEvent(t, fixture.stream)
	effect := recvRelayEvent(t, fixture.stream)
	completed := recvRelayEvent(t, fixture.stream)
	if accepted.Kind != EventRunAccepted || effect.Kind != EventEffectProposed || completed.Kind != EventRunCompleted {
		t.Fatalf("event order=%s,%s,%s", accepted.Kind, effect.Kind, completed.Kind)
	}
	if effect.Proposal == nil || effect.Proposal.Kind != "emoji" {
		t.Fatalf("sticker effect missing: %#v", effect)
	}
	var output domain.EmojiOutput
	if err := json.Unmarshal(effect.Proposal.Payload, &output); err != nil || string(output.Data) != "sticker-bytes" {
		t.Fatalf("emoji payload=%#v err=%v", output, err)
	}
}

func TestStickerCapabilityOrdersTextBeforeSticker(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)

	fixture.selectSticker(t)
	fixture.sendFinal(t, "文字回复")

	events := []Event{
		recvRelayEvent(t, fixture.stream),
		recvRelayEvent(t, fixture.stream),
		recvRelayEvent(t, fixture.stream),
		recvRelayEvent(t, fixture.stream),
	}
	want := []EventKind{EventRunAccepted, EventReplyProposed, EventEffectProposed, EventRunCompleted}
	for index := range want {
		if events[index].Kind != want[index] {
			t.Fatalf("event %d kind=%s, want %s", index, events[index].Kind, want[index])
		}
	}
}

func TestStickerCapabilityGroupObservationDiscardsStagedSticker(t *testing.T) {
	tests := []struct {
		input   string
		content string
	}{
		{input: "[group ambient]\nsender: User\nmessage: hello", content: relayObserveToken},
		{
			input:   "[group addressed]\nsender: User\nmessage: hello",
			content: "[others chatting; no @-mention or question for me — staying silent]",
		},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			fixture := newStickerRelayFixture(t, test.input)
			defer fixture.close(t)

			fixture.selectSticker(t)
			fixture.sendFinal(t, test.content)

			accepted := recvRelayEvent(t, fixture.stream)
			completed := recvRelayEvent(t, fixture.stream)
			if accepted.Kind != EventRunAccepted || completed.Kind != EventRunCompleted || completed.Sequence != 2 {
				t.Fatalf("group observation events=%#v %#v", accepted, completed)
			}
		})
	}
}

func TestStickerCapabilityAllowsMultipleSelections(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)

	fixture.selectSticker(t)
	fixture.selectSticker(t)
	fixture.sendFinal(t, relayEffectOnlyToken)

	want := []EventKind{
		EventRunAccepted,
		EventEffectProposed,
		EventEffectProposed,
		EventRunCompleted,
	}
	for index, kind := range want {
		event := recvRelayEvent(t, fixture.stream)
		if event.Kind != kind {
			t.Fatalf("event %d kind=%s, want %s", index, event.Kind, kind)
		}
		if kind == EventEffectProposed && (event.Proposal == nil || event.Proposal.Kind != "emoji") {
			t.Fatalf("event %d effect missing: %#v", index, event)
		}
	}
}

func TestRelayRejectsObservationTokenForDirectReply(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)

	writeRelayFrame(t, fixture.connection, map[string]any{
		"type": "outbound", "requestId": "request-observe-direct",
		"action": map[string]any{
			"op": "send", "chat_id": relayChatID(fixture.request), "content": "`" + relayObserveToken + "`",
			"metadata": map[string]any{"notify": true},
		},
	})
	result := readRelayFrame(t, fixture.connection)
	body, _ := result["result"].(map[string]any)
	if body["success"] != false || body["error"] != "observation is not valid for an addressed message" {
		t.Fatalf("direct observation token was not rejected: %#v", result)
	}
}

func TestRelayRejectsUnknownInternalCompletionToken(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)

	writeRelayFrame(t, fixture.connection, map[string]any{
		"type": "outbound", "requestId": "request-unknown-token",
		"action": map[string]any{
			"op": "send", "chat_id": relayChatID(fixture.request),
			"content":  "[[GOLEM_HERMES_EFFEXT_ONLY_V1]]",
			"metadata": map[string]any{"notify": true},
		},
	})
	result := readRelayFrame(t, fixture.connection)
	body, _ := result["result"].(map[string]any)
	if body["success"] != false || body["error"] != "unsupported internal completion token" {
		t.Fatalf("unknown internal token result=%#v", result)
	}
}
