package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golem_plugin_hermes/internal/domain"

	"github.com/coder/websocket"
)

type stickerRelayFixture struct {
	server     *httptest.Server
	connection *websocket.Conn
	stream     Stream
	request    RunRequest
	stickers   *fakeStickerCapability
}

func newStickerRelayFixture(t *testing.T, input string) *stickerRelayFixture {
	t.Helper()
	stickers := &fakeStickerCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken,
		Stickers:        stickers,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := newStickerRelayServer(gateway)
	connection, _, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(server.URL, "http")+gateway.config.Path,
		nil,
	)
	if err != nil {
		server.Close()
		t.Fatalf("Dial: %v", err)
	}
	writeRelayFrame(t, connection, map[string]any{
		"type": "hello", "platform": "relay", "botId": "golem",
	})
	_ = readRelayFrame(t, connection)
	request := stickerRunRequest(input)
	stream, err := gateway.Start(context.Background(), request)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "start failed")
		server.Close()
		t.Fatalf("Start: %v", err)
	}
	_ = readRelayFrame(t, connection)
	return &stickerRelayFixture{
		server: server, connection: connection, stream: stream, request: request, stickers: stickers,
	}
}

func newStickerRelayServer(gateway *RelayGateway) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(gateway.config.Path, gateway.serveRelay)
	mux.HandleFunc(stickerSearchPath, gateway.serveStickerSearch)
	mux.HandleFunc(stickerMaterializePath, gateway.serveStickerMaterialize)
	mux.HandleFunc(stickerSelectPath, gateway.serveStickerSelect)
	mux.HandleFunc(stickerSelectManyPath, gateway.serveStickerSelectMany)
	return httptest.NewServer(mux)
}

func stickerRunRequest(input string) RunRequest {
	request := RunRequest{
		RunID: "run-sticker", SessionID: "chatroom:room-1", Lane: domain.LaneInteractive,
		Principal: domain.Principal{ID: "wxid-owner", Name: "Owner"},
		Input:     input, ChatType: "group", MessageID: "event-sticker",
		TriggerKind: domain.TriggerExplicit,
	}
	if strings.HasPrefix(input, "[direct]") {
		request.SessionID = "private:wxid-owner"
		request.ChatType = "dm"
	}
	return request
}

func (f *stickerRelayFixture) close(t *testing.T) {
	t.Helper()
	_ = f.stream.Close()
	_ = f.connection.Close(websocket.StatusNormalClosure, "test complete")
	f.server.Close()
}

func (f *stickerRelayFixture) selectSticker(t *testing.T) {
	t.Helper()
	status, response := postCapability(
		t,
		f.server.URL+stickerSelectPath,
		stickerSelectRequest{
			CandidateID: "candidate-1", Context: capabilityContext(f.request),
		},
	)
	if status != http.StatusOK || response["staged"] != true {
		t.Fatalf("select status=%d response=%v", status, response)
	}
}

func (f *stickerRelayFixture) sendFinal(t *testing.T, content string) {
	t.Helper()
	writeRelayFrame(t, f.connection, map[string]any{
		"type": "outbound", "requestId": "request-final",
		"action": map[string]any{
			"op": "send", "chat_id": relayChatID(f.request), "content": content,
			"metadata": map[string]any{"notify": true},
		},
	})
	result := readRelayFrame(t, f.connection)
	body, _ := result["result"].(map[string]any)
	if body["success"] != true {
		t.Fatalf("final response=%#v", result)
	}
}

func capabilityContext(request RunRequest) capabilitySessionContext {
	chatID := relayChatID(request)
	return capabilitySessionContext{
		Platform: "relay", ChatID: chatID, UserID: request.Principal.ID,
		SessionKey: relaySessionKey(request, chatID),
		SessionID:  request.SessionID, MessageID: request.MessageID,
	}
}

func postCapability(t *testing.T, target string, value any) (int, map[string]any) {
	t.Helper()
	return postCapabilityWithToken(t, capabilityPost{
		target: target, token: testCapabilityToken, value: value,
	})
}

func postCapabilityWithoutAuth(t *testing.T, target string, value any) (int, map[string]any) {
	t.Helper()
	return postCapabilityWithToken(t, capabilityPost{target: target, value: value})
}

type capabilityPost struct {
	target string
	token  string
	value  any
}

func postCapabilityWithToken(t *testing.T, post capabilityPost) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(post.value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, post.target, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if post.token != "" {
		request.Header.Set("Authorization", "Bearer "+post.token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return response.StatusCode, body
}

func recvRelayEvent(t *testing.T, stream Stream) Event {
	t.Helper()
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return event
}
