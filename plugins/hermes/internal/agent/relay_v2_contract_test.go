package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"

	"github.com/coder/websocket"
)

type relayResultMemoryStore struct {
	mu     sync.Mutex
	values map[string]domain.RelayRunResult
}

func (s *relayResultMemoryStore) GetRelayRunResult(_ context.Context, id string) (domain.RelayRunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[id]
	if !ok {
		return domain.RelayRunResult{}, storeport.ErrNotFound
	}
	return value, nil
}
func (s *relayResultMemoryStore) GetRelayInvocationStatus(
	_ context.Context,
	invocationID string,
) (domain.RelayInvocationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.values {
		if value.InvocationID == invocationID {
			return domain.RelayInvocationStatus{
				InvocationID: invocationID, RunID: value.RunID, RunState: domain.RunSucceeded,
				ProposalID: value.ProposalID, ResultKind: value.ResultKind,
				ResultHash: value.ResultHash, OutboxIDs: append([]string(nil), value.OutboxIDs...),
			}, nil
		}
	}
	return domain.RelayInvocationStatus{}, storeport.ErrNotFound
}
func (s *relayResultMemoryStore) put(value domain.RelayRunResult) {
	s.mu.Lock()
	s.values[value.ProposalID] = value
	s.mu.Unlock()
}

func resultHash(t *testing.T, invocationID, proposalID, kind string, content any, effects []OutputProposal) string {
	t.Helper()
	canonical, err := domain.CanonicalJSON(map[string]any{"op": "commit_run_result_v1",
		"invocation_id": invocationID, "proposal_id": proposalID, "result_kind": kind,
		"content": content, "effects": effects})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func startV2Relay(t *testing.T, request RunRequest) (*RelayGateway, *relayResultMemoryStore, *websocket.Conn, Stream) {
	t.Helper()
	results := &relayResultMemoryStore{values: make(map[string]domain.RelayRunResult)}
	gateway, err := NewRelayGateway(RelayConfig{ObservationV2Enabled: true, RunResults: results})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveRelay))
	t.Cleanup(server.Close)
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	writeRelayFrame(t, connection, map[string]any{"type": "hello", "observation_protocol_version": 1,
		"supports_observe_batch_v1": true, "supports_invoke_observation_v1": true,
		"supports_durable_run_result_v1": true, "supports_verified_actor_v1": true,
		"supports_run_terminated_v1": true})
	_ = readRelayFrame(t, connection)
	type startResult struct {
		stream Stream
		err    error
	}
	done := make(chan startResult, 1)
	go func() {
		stream, startErr := gateway.Start(context.Background(), request)
		done <- startResult{stream, startErr}
	}()
	invoke := readRelayFrame(t, connection)
	if invoke["type"] != "invoke_observation_v1" {
		t.Fatalf("invoke=%#v", invoke)
	}
	writeRelayFrame(t, connection, map[string]any{"type": "invocation_ack_v1", "request_id": request.RunID,
		"invocation_id": request.InvocationID, "status": "admitted",
		"durable_through_conversation_seq": request.RequiredContextSeq})
	started := <-done
	if started.err != nil {
		t.Fatal(started.err)
	}
	t.Cleanup(func() { _ = started.stream.Close() })
	accepted, err := started.stream.Recv(context.Background())
	if err != nil || accepted.Kind != EventRunAccepted {
		t.Fatalf("accepted=%#v err=%v", accepted, err)
	}
	return gateway, results, connection, started.stream
}

func TestRelayDescriptorObservationV2FollowsConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
	}{
		{name: "disabled", enabled: false},
		{name: "enabled", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway, err := NewRelayGateway(RelayConfig{ObservationV2Enabled: test.enabled,
				RecentRawMessages: 10, MaxProjectionTokens: 4000})
			if err != nil {
				t.Fatal(err)
			}
			descriptor := relayDescriptor(relayDescriptorOptions{observationV2: gateway.config.ObservationV2Enabled,
				recentRawMessages:   gateway.config.RecentRawMessages,
				maxProjectionTokens: gateway.config.MaxProjectionTokens})
			wantVersion := 0
			if test.enabled {
				wantVersion = 1
			}
			if got := descriptor["observation_protocol_version"]; got != wantVersion {
				t.Fatalf("protocol version=%v, want %d", got, wantVersion)
			}
			for _, key := range []string{"supports_observe_batch_v1", "supports_invoke_observation_v1",
				"supports_durable_run_result_v1", "supports_verified_actor_v1", "supports_run_terminated_v1"} {
				if got := descriptor[key]; got != test.enabled {
					t.Fatalf("%s=%v, want %v", key, got, test.enabled)
				}
			}
			wantMessages, wantTokens := 0, 0
			if test.enabled {
				wantMessages, wantTokens = 10, 4000
			}
			if descriptor["recent_raw_messages"] != wantMessages || descriptor["max_projection_tokens"] != wantTokens {
				t.Fatalf("projection limits=%v/%v, want %d/%d", descriptor["recent_raw_messages"],
					descriptor["max_projection_tokens"], wantMessages, wantTokens)
			}
		})
	}
}

func TestRelayInvokeTrustedCommandRequiresOwner(t *testing.T) {
	base := RunRequest{RunID: "run-1", SessionID: "private:user", Lane: domain.LaneInteractive,
		Input: "/new", InvocationID: "invoke-1"}
	participant := relayInvokeObservation(base, "chat", nil)
	if _, exists := participant["trusted_command"]; exists {
		t.Fatal("participant command was trusted")
	}
	base.Principal.IsOwner = true
	owner := relayInvokeObservation(base, "chat", nil)
	if got := owner["trusted_command"]; got != "/new" {
		t.Fatalf("trusted_command=%v", got)
	}
	base.Input = "ordinary text"
	if _, exists := relayInvokeObservation(base, "chat", nil)["trusted_command"]; exists {
		t.Fatal("ordinary owner text was emitted as trusted command")
	}
}

func TestRelayPresentsMaterializedEmojiAsVisualInput(t *testing.T) {
	request := RunRequest{
		RunID: "run-emoji", SessionID: "chatroom:room", Lane: domain.LaneInteractive,
		Media: []domain.InboundMedia{{Kind: "emoji", URL: "https://media.example/sticker.jpg"}},
	}
	frame := relayInvokeObservation(request, "chat", []string{"https://media.example/sticker.jpg"})
	media, ok := frame["media"].([]map[string]any)
	if !ok || len(media) != 1 || media[0]["kind"] != "image" {
		t.Fatalf("relay emoji media=%#v", frame["media"])
	}
	legacy := relayInboundEvent(request, "chat", []string{"https://media.example/sticker.jpg"})
	if legacy["message_type"] != "photo" {
		t.Fatalf("legacy emoji message_type=%v", legacy["message_type"])
	}
}

func TestRelayInvokeCarriesCurrentObservationOnlyForLagFallback(t *testing.T) {
	observation := &domain.ConversationObservation{ObservationID: "o3", ConversationSeq: 3, PayloadHash: "hash"}
	base := RunRequest{RunID: "run-lag", SessionID: "chatroom:room", Input: "hello",
		InvocationID: "invoke-lag", ConversationID: "c1", CurrentObservationID: "o3",
		CurrentPayloadHash: "hash", RequiredContextSeq: 3, CurrentObservation: observation}
	if frame := relayInvokeObservation(base, "chat", nil); frame["allow_context_lag"] != nil || frame["current_observation"] != nil {
		t.Fatalf("ordinary invocation leaked lag payload: %#v", frame)
	}
	base.ContextLagFallback = true
	frame := relayInvokeObservation(base, "chat", nil)
	if frame["allow_context_lag"] != true || frame["current_observation"] != observation {
		t.Fatalf("lag invocation frame=%#v", frame)
	}
}

func TestInboundRunTerminatedDoesNotCloseOrDowngradeConnection(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{ObservationV2Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	connection := &relayConnection{negotiated: true, v2: true}
	if err := gateway.handleFrame(context.Background(), connection,
		[]byte(`{"type":"run_terminated_v1","invocation_id":"invoke-1","status":"completed"}`)); err != nil {
		t.Fatalf("handle run_terminated: %v", err)
	}
	if !connection.negotiated || !connection.v2 {
		t.Fatalf("connection changed: %#v", connection)
	}
}

func TestV2VisibleAndObserveResultHashInterop(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		content    any
		visible    bool
	}{
		{name: "visible", kind: "visible_reply", content: "hello", visible: true},
		{name: "observe", kind: "observe", content: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := RunRequest{RunID: "run-" + test.name, SessionID: "chatroom:room", Lane: domain.LaneInteractive,
				Input: "ambient", ChatType: "group", ConversationID: "wechat:group:room",
				CurrentObservationID: "obs-1", CurrentPayloadHash: "payload", RequiredContextSeq: 1,
				InvocationID: "invoke-" + test.name}
			_, results, connection, stream := startV2Relay(t, request)
			proposalID := "proposal-" + test.name
			hash := resultHash(t, request.InvocationID, proposalID, test.kind, test.content, []OutputProposal{})
			writeRelayFrame(t, connection, map[string]any{"type": "outbound", "requestId": "request-commit",
				"action": map[string]any{"op": "commit_run_result_v1", "invocation_id": request.InvocationID,
					"proposal_id": proposalID, "result_kind": test.kind, "content": test.content,
					"effects": []any{}, "result_hash": hash}})
			if test.visible {
				event, err := stream.Recv(context.Background())
				if err != nil || event.Kind != EventReplyProposed || event.Text != "hello" {
					t.Fatalf("reply=%#v err=%v", event, err)
				}
			}
			completed, err := stream.Recv(context.Background())
			if err != nil || completed.Kind != EventRunCompleted || completed.ResultHash != hash {
				t.Fatalf("completed=%#v err=%v", completed, err)
			}
			results.put(domain.RelayRunResult{ProposalID: proposalID, InvocationID: request.InvocationID,
				RunID: request.RunID, ResultKind: test.kind, ResultHash: hash, OutboxIDs: []string{"outbox-1"}})
			if err := stream.Send(context.Background(), Command{Kind: CommandProposalResult, RunID: request.RunID,
				ProposalID: proposalID}); err != nil {
				t.Fatal(err)
			}
			result := readRelayFrame(t, connection)
			body := result["result"].(map[string]any)
			if body["success"] != true || body["result_hash"] != hash {
				t.Fatalf("result=%#v", result)
			}
			writeRelayFrame(t, connection, map[string]any{
				"type": "golem_invocation_status_v1", "request_id": "status-1",
				"invocation_id": request.InvocationID,
			})
			status := readRelayFrame(t, connection)
			if status["type"] != "golem_invocation_status_result_v1" ||
				status["status"] != string(domain.RunSucceeded) || status["proposal_id"] != proposalID ||
				status["result_hash"] != hash {
				t.Fatalf("invocation status=%#v", status)
			}
		})
	}
}

func TestV2VisibleResultStripsInternalCompletionToken(t *testing.T) {
	request := RunRequest{RunID: "run-visible-sanitized", SessionID: "chatroom:room", Lane: domain.LaneInteractive,
		Input: "ambient", ChatType: "group", ConversationID: "wechat:group:room",
		CurrentObservationID: "obs-1", CurrentPayloadHash: "payload", RequiredContextSeq: 1,
		InvocationID: "invoke-visible-sanitized"}
	_, results, connection, stream := startV2Relay(t, request)
	unknownToken := "[[GOLEM_HERMES_FUTURE_V2]]"
	content := "hello\n\n`" + relayObserveToken + "`。\n" + unknownToken
	proposalID := "proposal-visible-sanitized"
	hash := resultHash(t, request.InvocationID, proposalID, "visible_reply", content, []OutputProposal{})
	writeRelayFrame(t, connection, map[string]any{"type": "outbound", "requestId": "request-sanitized",
		"action": map[string]any{"op": "commit_run_result_v1", "invocation_id": request.InvocationID,
			"proposal_id": proposalID, "result_kind": "visible_reply", "content": content,
			"effects": []any{}, "result_hash": hash}})

	reply, err := stream.Recv(context.Background())
	if err != nil || reply.Kind != EventReplyProposed || reply.Text != "hello" || reply.Proposal == nil ||
		strings.Contains(string(reply.Proposal.Payload), relayObserveToken) ||
		strings.Contains(string(reply.Proposal.Payload), unknownToken) {
		t.Fatalf("reply=%#v err=%v", reply, err)
	}
	completed, err := stream.Recv(context.Background())
	if err != nil || completed.Kind != EventRunCompleted || completed.ResultHash != hash {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	results.put(domain.RelayRunResult{ProposalID: proposalID, InvocationID: request.InvocationID,
		RunID: request.RunID, ResultKind: "visible_reply", ResultHash: hash, OutboxIDs: []string{"outbox-1"}})
	if err := stream.Send(context.Background(), Command{Kind: CommandProposalResult, RunID: request.RunID,
		ProposalID: proposalID}); err != nil {
		t.Fatal(err)
	}
	result := readRelayFrame(t, connection)
	body := result["result"].(map[string]any)
	if body["success"] != true || body["result_hash"] != hash {
		t.Fatalf("result=%#v", result)
	}
}

func TestV2InvalidRequiredObserveFailsRunAndReleasesChatSlot(t *testing.T) {
	request := RunRequest{RunID: "run-required", SessionID: "chatroom:room", Lane: domain.LaneInteractive,
		Input: "@ccff hello", ChatType: "group", ConversationID: "wechat:group:room",
		CurrentObservationID: "obs-1", CurrentPayloadHash: "payload", RequiredContextSeq: 1,
		InvocationID: "invoke-required", RequireVisibleReply: true}
	gateway, _, connection, stream := startV2Relay(t, request)
	proposalID := "proposal-invalid-observe"
	hash := resultHash(t, request.InvocationID, proposalID, "observe", nil, []OutputProposal{})
	writeRelayFrame(t, connection, map[string]any{"type": "outbound", "requestId": "request-invalid",
		"action": map[string]any{"op": "commit_run_result_v1", "invocation_id": request.InvocationID,
			"proposal_id": proposalID, "result_kind": "observe", "content": nil,
			"effects": []any{}, "result_hash": hash}})

	failed, err := stream.Recv(context.Background())
	if err != nil || failed.Kind != EventRunFailed || failed.Err == nil ||
		!strings.Contains(failed.Err.Error(), "invalid observe result") {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	result := readRelayFrame(t, connection)
	body := result["result"].(map[string]any)
	if result["type"] != "outbound_result" || body["success"] != false ||
		body["error"] != "invalid observe result" {
		t.Fatalf("result=%#v", result)
	}
	if pending, _ := gateway.pendingRunForChat(relayChatID(request), request.InvocationID); pending != nil {
		t.Fatal("invalid durable proposal kept the chat admission slot")
	}
}

func TestV2ProgressDoesNotCompleteAndStagedEffectIsMerged(t *testing.T) {
	request := RunRequest{RunID: "run-progress", SessionID: "private:user", Lane: domain.LaneInteractive,
		Input: "hello", ChatType: "dm", ConversationID: "wechat:dm:user", CurrentObservationID: "obs",
		CurrentPayloadHash: "payload", RequiredContextSeq: 1, InvocationID: "invoke-progress"}
	gateway, results, connection, stream := startV2Relay(t, request)
	writeRelayFrame(t, connection, map[string]any{"type": "outbound", "requestId": "progress",
		"action": map[string]any{"op": "send", "chat_id": relayChatID(request), "content": "working",
			"metadata": map[string]any{"notify": false}}})
	if body := readRelayFrame(t, connection)["result"].(map[string]any); body["success"] != true {
		t.Fatalf("progress=%#v", body)
	}
	progress, err := stream.Recv(context.Background())
	if err != nil || progress.Kind != EventProgress {
		t.Fatalf("progress event=%#v err=%v", progress, err)
	}

	effect := OutputProposal{Kind: "emoji", Payload: []byte(`{"url":"https://example.test/e.gif"}`)}
	run, _ := gateway.pendingRunForChat(relayChatID(request), request.InvocationID)
	if run == nil {
		t.Fatal("progress completed the run")
	}
	if err := run.stageEffect(effect); err != nil {
		t.Fatal(err)
	}
	proposalID := "proposal-effect"
	hash := resultHash(t, request.InvocationID, proposalID, "effect_only", nil, []OutputProposal{})
	writeRelayFrame(t, connection, map[string]any{"type": "outbound", "requestId": "effect",
		"action": map[string]any{"op": "commit_run_result_v1", "invocation_id": request.InvocationID,
			"proposal_id": proposalID, "result_kind": "effect_only", "content": nil,
			"effects": []any{}, "result_hash": hash}})
	effectEvent, err := stream.Recv(context.Background())
	if err != nil || effectEvent.Kind != EventEffectProposed || effectEvent.Proposal == nil {
		t.Fatalf("effect=%#v err=%v", effectEvent, err)
	}
	completed, err := stream.Recv(context.Background())
	if err != nil || completed.Kind != EventRunCompleted {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	results.put(domain.RelayRunResult{ProposalID: proposalID, InvocationID: request.InvocationID,
		RunID: request.RunID, ResultKind: "effect_only", ResultHash: hash})
	if err := stream.Send(context.Background(), Command{Kind: CommandProposalResult, RunID: request.RunID,
		ProposalID: proposalID}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = readRelayFrame(t, connection)
}

func TestV2TerminalProposalReleasesRunBeforeReceipt(t *testing.T) {
	request := RunRequest{RunID: "run-disconnect", SessionID: "private:user", Lane: domain.LaneInteractive,
		Input: "hello", ChatType: "dm", ConversationID: "wechat:dm:user", CurrentObservationID: "obs",
		CurrentPayloadHash: "payload", RequiredContextSeq: 1, InvocationID: "invoke-disconnect"}
	gateway, _, _, stream := startV2Relay(t, request)

	proposalID := "proposal-disconnect"
	hash := resultHash(t, request.InvocationID, proposalID, "visible_reply", "hello", []OutputProposal{})
	gateway.mu.Lock()
	serverConnection := gateway.conn
	gateway.mu.Unlock()
	if serverConnection == nil {
		t.Fatal("relay connection unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- gateway.acceptDurableResult(ctx, serverConnection, "request-disconnect", request.InvocationID,
			proposalID, "visible_reply", hash, "hello", []OutputProposal{}, nil)
	}()
	if event, err := stream.Recv(context.Background()); err != nil || event.Kind != EventReplyProposed {
		t.Fatalf("reply=%#v err=%v", event, err)
	}
	if event, err := stream.Recv(context.Background()); err != nil || event.Kind != EventRunCompleted {
		t.Fatalf("completed=%#v err=%v", event, err)
	}

	if pending, _ := gateway.pendingRunForChat(relayChatID(request), request.InvocationID); pending != nil {
		t.Fatal("terminal proposal kept the chat admission slot before receipt")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("accept result error=%v, want context canceled", err)
	}
}
