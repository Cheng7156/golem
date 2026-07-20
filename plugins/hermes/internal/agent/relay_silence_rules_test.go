package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golem_plugin_hermes/internal/domain"

	"github.com/coder/websocket"
)

func TestRelaySilenceRulesReloadWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silence-rules.txt")
	writeSilenceRules(t, path, "# managed by Hermes\nprefix:custom-provider-omit:\n")
	gateway, err := NewRelayGateway(RelayConfig{SilenceRulesFile: path})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	first := "CUSTOM-PROVIDER-OMIT: unknown command"
	if !gateway.isObserveResponse(first) {
		t.Fatal("prefix rule did not match")
	}
	writeSilenceRules(t, path, "exact:[custom silence]\n")
	if gateway.isObserveResponse(first) {
		t.Fatal("stale prefix rule remained active after file update")
	}
	if !gateway.isObserveResponse("[CUSTOM SILENCE]") {
		t.Fatal("updated exact rule did not match case-insensitively")
	}
}

func TestRelaySilenceRuleCompletesRunWithoutReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silence-rules.txt")
	writeSilenceRules(t, path, "prefix:I don't need to respond to this\n")
	gateway, connection := startSilenceRelayGateway(t, path)
	stream := startSilenceRelayRun(t, gateway, connection)
	content := "I don't need to respond to this - unknown command. Staying silent."
	writeSilenceRelayReply(t, connection, content)
	assertCompletedWithoutReply(t, stream)
}

func TestRelaySilenceRulesRejectInvalidConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silence-rules.txt")
	writeSilenceRules(t, path, "prefix:\n")
	if _, err := NewRelayGateway(RelayConfig{SilenceRulesFile: path}); err == nil {
		t.Fatal("invalid silence rules were accepted")
	}
}

func TestRelayDescriptorDocumentsConfiguredSilenceRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silence-rules.txt")
	writeSilenceRules(t, path, "# initially empty\n")
	descriptor := relayDescriptor(relayDescriptorOptions{silenceRulesFile: path})
	hint, _ := descriptor["platform_hint"].(string)
	expected := []string{strconv.Quote(path), "exact:", "prefix:", "suffix:", "owner explicitly asks"}
	for _, value := range expected {
		if !strings.Contains(hint, value) {
			t.Fatalf("platform hint %q does not contain %q", hint, value)
		}
	}
}

func startSilenceRelayGateway(t *testing.T, path string) (*RelayGateway, *websocket.Conn) {
	t.Helper()
	gateway, err := NewRelayGateway(RelayConfig{SilenceRulesFile: path})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveRelay))
	connection, _, err := websocket.Dial(
		context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil,
	)
	if err != nil {
		server.Close()
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
		server.Close()
	})
	writeRelayFrame(t, connection, map[string]any{
		"type": "hello", "platform": "relay", "botId": "golem",
	})
	_ = readRelayFrame(t, connection)
	return gateway, connection
}

func startSilenceRelayRun(
	t *testing.T, gateway *RelayGateway, connection *websocket.Conn,
) Stream {
	t.Helper()
	stream, err := gateway.Start(context.Background(), RunRequest{
		RunID: "run-file-silence", SessionID: "chatroom:room-1",
		Lane: domain.LaneInteractive, Principal: domain.Principal{ID: "user-1"},
		Input: "[group ambient]\nsender: User\nmessage: test", ChatType: "group",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	_ = readRelayFrame(t, connection)
	return stream
}

func writeSilenceRelayReply(t *testing.T, connection *websocket.Conn, content string) {
	t.Helper()
	writeRelayFrame(t, connection, map[string]any{
		"type": "outbound", "requestId": "request-file-silence",
		"action": map[string]any{
			"op": "send", "chat_id": "chatroom:room-1|interactive", "content": content,
			"metadata": map[string]any{"notify": true},
		},
	})
	result := readRelayFrame(t, connection)
	body, _ := result["result"].(map[string]any)
	if body["success"] != true {
		t.Fatalf("final response=%#v", result)
	}
}

func assertCompletedWithoutReply(t *testing.T, stream Stream) {
	t.Helper()
	accepted, err := stream.Recv(context.Background())
	if err != nil || accepted.Kind != EventRunAccepted {
		t.Fatalf("accepted event=%#v err=%v", accepted, err)
	}
	completed, err := stream.Recv(context.Background())
	if err != nil || completed.Kind != EventRunCompleted {
		t.Fatalf("completed event=%#v err=%v", completed, err)
	}
	if completed.Proposal != nil || completed.Text != "" {
		t.Fatalf("silence produced visible reply: %#v", completed)
	}
}

func writeSilenceRules(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
}
