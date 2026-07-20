package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"

	"github.com/coder/websocket"
)

func TestRelayGatewayRoundTrip(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveRelay))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	connection, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")

	writeRelayFrame(t, connection, map[string]any{
		"type":     "hello",
		"platform": "relay",
		"botId":    "golem",
	})
	descriptor := readRelayFrame(t, connection)
	if descriptor["type"] != "descriptor" {
		t.Fatalf("unexpected descriptor frame: %#v", descriptor)
	}
	capabilities, ok := descriptor["descriptor"].(map[string]any)
	if !ok {
		t.Fatalf("descriptor capabilities missing: %#v", descriptor)
	}
	hint, _ := capabilities["platform_hint"].(string)
	for _, required := range []string{"ordinary final assistant text", "Do not search for or call MCP", "automatically delivers it through Golem", "group ambient", "Never explain that no reply is needed", relayObserveToken} {
		if !strings.Contains(hint, required) {
			t.Fatalf("platform_hint %q does not contain %q", hint, required)
		}
	}

	stream, err := gateway.Start(context.Background(), RunRequest{
		RunID:     "run-1",
		SessionID: "chatroom:room-1",
		Principal: domain.Principal{ID: "user-1", Name: "Tester"},
		Lane:      domain.LaneInteractive,
		Input:     "hello",
		ChatType:  "group",
		ChatName:  "Room",
		MessageID: "event-1",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stream.Close()
	inbound := readRelayFrame(t, connection)
	if inbound["type"] != "inbound" {
		t.Fatalf("unexpected inbound frame: %#v", inbound)
	}
	event := inbound["event"].(map[string]any)
	source := event["source"].(map[string]any)
	if event["text"] != "hello" || source["platform"] != "relay" || source["scope_id"] != "chatroom:room-1" {
		t.Fatalf("unexpected normalized event: %#v", event)
	}

	writeRelayFrame(t, connection, map[string]any{
		"type":      "outbound",
		"requestId": "request-1",
		"action": map[string]any{
			"op":      "send",
			"chat_id": "chatroom:room-1|interactive",
			"content": "**world**",
			"metadata": map[string]any{
				"notify": true,
			},
		},
	})
	result := readRelayFrame(t, connection)
	if result["type"] != "outbound_result" || result["requestId"] != "request-1" {
		t.Fatalf("unexpected outbound result: %#v", result)
	}

	kinds := []EventKind{EventRunAccepted, EventReplyProposed, EventRunCompleted}
	for index, expected := range kinds {
		event, recvErr := stream.Recv(context.Background())
		if recvErr != nil {
			t.Fatalf("Recv %d: %v", index, recvErr)
		}
		if event.Kind != expected || event.Sequence != uint64(index+1) || event.RunID != "run-1" {
			t.Fatalf("event %d=%#v, want kind=%s", index, event, expected)
		}
		if expected == EventReplyProposed && (event.Proposal == nil || event.Text != "world") {
			t.Fatalf("reply proposal missing: %#v", event)
		}
	}
}

func TestRelayRunWaitingForGatewayCanBeCancelled(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := RunRequest{
		RunID: "run-waiting", SessionID: "private:user-1", Input: "hello",
		Lane: domain.LaneInteractive,
	}
	done := make(chan error, 1)
	go func() {
		_, startErr := gateway.Start(context.Background(), request)
		done <- startErr
	}()

	deadline := time.Now().Add(time.Second)
	for {
		gateway.mu.Lock()
		pending := gateway.pending[relayChatID(request)]
		gateway.mu.Unlock()
		if pending != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run was not reserved while waiting for Gateway")
		}
		time.Sleep(time.Millisecond)
	}
	if err := gateway.CancelRun(context.Background(), request.RunID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting Relay Run did not stop after cancellation")
	}
	gateway.mu.Lock()
	_, exists := gateway.pending[relayChatID(request)]
	gateway.mu.Unlock()
	if exists {
		t.Fatal("cancelled waiting Run remained pending")
	}
}

func TestRelayCancelRunRejectsUnknownRun(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	err = gateway.CancelRun(context.Background(), "run-missing")
	if !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("CancelRun error=%v, want ErrRunNotActive", err)
	}
}

func TestRelayRejectsOrphanSendWhenAsyncDeliveryIsEnabled(t *testing.T) {
	capability := &fakeAsyncDeliveryCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken,
		AsyncDelivery:   capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveRelay))
	defer server.Close()
	connection, _, err := websocket.Dial(
		context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil,
	)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	writeRelayFrame(t, connection, map[string]any{"type": "hello"})
	_ = readRelayFrame(t, connection)
	writeRelayFrame(t, connection, map[string]any{
		"type": "outbound", "requestId": "orphan",
		"action": map[string]any{
			"op": "send", "chat_id": "chatroom:missing|interactive",
			"content": "late reply", "metadata": map[string]any{"notify": true},
		},
	})
	result := readRelayFrame(t, connection)
	body := result["result"].(map[string]any)
	if body["success"] != false || body["error"] != "no active run for chat" {
		t.Fatalf("orphan send result=%#v", result)
	}
}

func TestRelayObserveTokenCompletesWithoutReplyProposal(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveRelay))
	defer server.Close()
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")

	writeRelayFrame(t, connection, map[string]any{"type": "hello", "platform": "relay", "botId": "golem"})
	_ = readRelayFrame(t, connection)
	stream, err := gateway.Start(context.Background(), RunRequest{
		RunID: "run-observe", SessionID: "chatroom:room-1", Lane: domain.LaneInteractive,
		Principal: domain.Principal{ID: "user-1"}, Input: "[group ambient]\nsender: User (user-1)\nmessage: ambient", ChatType: "group",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stream.Close()
	_ = readRelayFrame(t, connection)
	writeRelayFrame(t, connection, map[string]any{
		"type": "outbound", "requestId": "request-progress",
		"action": map[string]any{
			"op": "send", "chat_id": "chatroom:room-1|interactive", "content": "intermediate",
			"metadata": map[string]any{"notify": false},
		},
	})
	progressResult := readRelayFrame(t, connection)
	progressBody, _ := progressResult["result"].(map[string]any)
	if progressBody["success"] != true {
		t.Fatalf("unexpected deferred progress result: %#v", progressResult)
	}
	writeRelayFrame(t, connection, map[string]any{
		"type": "outbound", "requestId": "request-observe",
		"action": map[string]any{
			"op": "send", "chat_id": "chatroom:room-1|interactive", "content": "不需要回复。",
			"metadata": map[string]any{"notify": true},
		},
	})
	result := readRelayFrame(t, connection)
	resultBody, _ := result["result"].(map[string]any)
	if result["type"] != "outbound_result" || resultBody["success"] != true {
		t.Fatalf("unexpected observe result: %#v", result)
	}

	accepted, err := stream.Recv(context.Background())
	if err != nil || accepted.Kind != EventRunAccepted {
		t.Fatalf("accepted event=%#v err=%v", accepted, err)
	}
	completed, err := stream.Recv(context.Background())
	if err != nil || completed.Kind != EventRunCompleted || completed.Proposal != nil || completed.Text != "" {
		t.Fatalf("completed event=%#v err=%v", completed, err)
	}
}

func TestRelayObserveResponseRecognition(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{content: relayObserveToken, want: true},
		{content: "`" + relayObserveToken + "`。", want: true},
		{content: " 不需要回复。 ", want: true},
		{content: "**无需回复！**", want: true},
		{content: "NO_REPLY", want: true},
		{content: "silent", want: true},
		{content: "[silence]", want: true},
		{content: "[others conversing about bot behavior; no question or @-mention directed at me — staying silent]", want: true},
		{content: "[others chatting; no @-mention or question for me — staying silent]", want: true},
		{content: "[Document from the user, but I'm unable to access its content. No question directed at me — staying silent.]", want: true},
		{content: "(Response formatting failed, plain text:)\n\n不需要回复。", want: true},
		{content: "(Response formatting failed, plain text:)\n\n" + relayObserveToken, want: true},
		{content: "(Response formatting failed, plain text:)\n\n正常回复", want: false},
		{content: "Response formatting failed, plain text:\n\n不需要回复。", want: false},
		{content: "我认为这条消息不需要回复。", want: false},
		{content: "不需要回复，但可以补充一点信息", want: false},
		{content: "They asked why the bot is staying silent.", want: false},
		{content: "[They asked why the bot is staying silent, so I explained the issue]", want: false},
		{content: "[Everyone please stay silent]", want: false},
		{content: "I don't need to respond to this. Staying silent.", want: false},
		{content: "I don't need to respond with silence; I should explain the error.", want: false},
		{content: "正常回复", want: false},
	}
	for _, test := range tests {
		if got := isObserveResponse(test.content); got != test.want {
			t.Errorf("isObserveResponse(%q)=%t, want %t", test.content, got, test.want)
		}
	}
}

func TestRelayUnwrapsHermesPlainTextFallback(t *testing.T) {
	tests := []struct {
		content string
		want    string
	}{
		{content: "(Response formatting failed, plain text:)\n\n正常回复", want: "正常回复"},
		{content: "  正常回复  ", want: "正常回复"},
		{content: "Response formatting failed, plain text:\n\n正常回复", want: "Response formatting failed, plain text:\n\n正常回复"},
	}
	for _, test := range tests {
		if got := unwrapHermesPlainTextFallback(test.content); got != test.want {
			t.Errorf("unwrapHermesPlainTextFallback(%q)=%q, want %q", test.content, got, test.want)
		}
	}
}

func TestRelayInternalTokenRecognitionRejectsAdditionalText(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{content: relayEffectOnlyToken, want: true},
		{content: "**" + relayEffectOnlyToken + "**", want: true},
		{content: relayEffectOnlyToken + "!", want: true},
		{content: "[[GOLEM_HERMES EFFECT_ONLY_V1]] ]", want: true},
		{content: "[[golem-hermes-effect-only-v1]]", want: true},
		{content: "use " + relayEffectOnlyToken, want: false},
		{content: relayEffectOnlyToken + " and text", want: false},
		{content: "[[GOLEM_HERMES EFFECT_ONLY_V2]]", want: false},
	}
	for _, test := range tests {
		if got := isInternalTokenResponse(test.content, relayEffectOnlyToken); got != test.want {
			t.Errorf("isInternalTokenResponse(%q)=%t, want %t", test.content, got, test.want)
		}
	}
}

func TestRelayMarkdownToWeChatText(t *testing.T) {
	input := "# 标题\n\n> **重点**\n\n* 项目与[文档](https://example.com)\n\n```go\nfmt.Println(`ok`)\n```\n\n~~旧内容~~"
	want := "标题\n\n重点\n\n- 项目与文档 (https://example.com)\n\nfmt.Println(`ok`)\n\n旧内容"
	if got := markdownToWeChatText(input); got != want {
		t.Fatalf("markdownToWeChatText()=%q, want %q", got, want)
	}
}

func TestRelayMarkdownOnlyReplyIsRejected(t *testing.T) {
	if _, _, err := newRelayTextProposal("---"); err == nil {
		t.Fatal("Markdown-only reply was accepted as empty visible text")
	}
}

func TestRelayGatewayRejectsNonLoopbackWithoutAuthentication(t *testing.T) {
	if _, err := NewRelayGateway(RelayConfig{ListenAddress: "0.0.0.0:8789"}); err == nil {
		t.Fatal("expected unauthenticated non-loopback listener to be rejected")
	}
}

func TestRelayGatewayAcceptsOfficialUpgradeTokenVector(t *testing.T) {
	const (
		secret = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
		token  = "Z3ctaW5zdGFuY2UtMTowOjM3YWE3YjE0NWU4NzY0ZDQwM2JhOWM2MzlmMjMwZGQ2M2RlOGVkOTliODhmZWQzNmFhMDI2MjVhOGE3ZTM1NjQ"
	)
	gateway, err := NewRelayGateway(RelayConfig{GatewayID: "gw-instance-1", SharedSecret: secret})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	if !gateway.authorized("Bearer "+token, time.Unix(1_750_000_000, 0)) {
		t.Fatal("official Hermes connector token vector was rejected")
	}
	if !gateway.authorized("Bearer "+token, time.Unix(1_750_000_000, 0).Add(time.Second)) {
		t.Fatal("non-expiring official token unexpectedly expired")
	}
	if gateway.authorized("Bearer "+makeRelayToken("gw-instance-1", secret, 1), time.Unix(2, 0)) {
		t.Fatal("expired relay token was accepted")
	}
	if gateway.authorized("Bearer "+makeRelayToken("other-gateway", secret, 0), time.Now()) {
		t.Fatal("token for a different gateway was accepted")
	}
}

func TestRelayGatewayMaterializesInboundMediaBySHA256(t *testing.T) {
	directory := t.TempDir()
	gateway, err := NewRelayGateway(RelayConfig{MediaDirectory: directory})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	urls, err := gateway.materializeMedia(context.Background(), []domain.InboundMedia{{
		Kind:     "image",
		Data:     data,
		MIMEType: "image/png",
	}})
	if err != nil {
		t.Fatalf("materializeMedia: %v", err)
	}
	digest := sha256.Sum256(data)
	want, err := filepath.Abs(filepath.Join(directory, hex.EncodeToString(digest[:])+".png"))
	if err != nil {
		t.Fatalf("absolute path: %v", err)
	}
	if len(urls) != 1 || urls[0] != want {
		t.Fatalf("materialized URLs=%v, want %q", urls, want)
	}
	stored, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read materialized media: %v", err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatal("materialized media bytes changed")
	}
	again, err := gateway.materializeMedia(context.Background(), []domain.InboundMedia{{Data: data, MIMEType: "image/png"}})
	if err != nil || len(again) != 1 || again[0] != want {
		t.Fatalf("idempotent materialization=%v err=%v", again, err)
	}
}

func TestRelayInterruptMatchesExactSession(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	secondContext, cancelSecond := context.WithCancel(context.Background())
	defer cancelFirst()
	defer cancelSecond()
	firstRequest := RunRequest{RunID: "run-1", SessionID: "private:user", Lane: domain.LaneInteractive}
	secondRequest := RunRequest{RunID: "run-2", SessionID: "private:user-extra", Lane: domain.LaneInteractive}
	first := &relayRun{request: firstRequest, chatID: relayChatID(firstRequest), cancel: cancelFirst}
	second := &relayRun{request: secondRequest, chatID: relayChatID(secondRequest), cancel: cancelSecond}
	gateway.pending[first.chatID] = first
	gateway.pending[second.chatID] = second

	gateway.interruptBySession(relaySessionKey(firstRequest, first.chatID))
	select {
	case <-firstContext.Done():
	default:
		t.Fatal("target relay run was not interrupted")
	}
	select {
	case <-secondContext.Done():
		t.Fatal("sibling relay run was interrupted by a partial session match")
	default:
	}
}

func TestRelayGroupSessionKeyIsSharedAcrossParticipants(t *testing.T) {
	first := RunRequest{
		SessionID: "chatroom:room-1", Lane: domain.LaneInteractive, ChatType: "group",
		Principal: domain.Principal{ID: "user-1"},
	}
	second := first
	second.Principal.ID = "user-2"
	firstKey := relaySessionKey(first, relayChatID(first))
	secondKey := relaySessionKey(second, relayChatID(second))
	if firstKey != secondKey {
		t.Fatalf("group session keys are not shared: %q != %q", firstKey, secondKey)
	}
}

func TestRelayAmbientDetectionDoesNotSuppressDirectOrAddressedReplies(t *testing.T) {
	tests := []RunRequest{
		{ChatType: "dm", Input: "[direct]\nmessage: hello"},
		{ChatType: "group", Input: "[group addressed]\nmessage: hello"},
	}
	for _, request := range tests {
		if (&relayRun{request: request}).isAmbientGroup() {
			t.Fatalf("non-ambient request was classified ambient: %#v", request)
		}
	}
}

func makeRelayToken(payload, secret string, expires int64) string {
	signed := payload + ":" + fmt.Sprint(expires)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signed))
	raw := signed + ":" + hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func writeRelayFrame(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, append(data, '\n')); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func readRelayFrame(t *testing.T, connection *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &value); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return value
}
