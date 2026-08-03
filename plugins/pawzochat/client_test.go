package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestReplyUsesBridgeProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bridge/golem/messages" || r.Method != http.MethodPost {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if actual := r.Header.Get("Authorization"); actual != "Bearer token" {
			t.Errorf("Authorization=%q", actual)
		}
		var request bridgeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("Decode request: %v", err)
		}
		if request.PersonaID != "persona" || request.SessionKey != "private:wxid" ||
			request.SessionName != "好友昵称" {
			t.Errorf("request=%#v", request)
		}
		if request.RequestID == "" || request.DeadlineUnixMS <= time.Now().UnixMilli() {
			t.Errorf("request correlation=%#v", request)
		}
		if !request.ForceReply {
			t.Error("private message did not force a reply")
		}
		var sender struct {
			Text string `json:"text"`
		}
		decodeEnvelopeSection(t, request.Text, "untrusted_message_from_sender_json", &sender)
		if sender.Text != "hello" {
			t.Errorf("sender text=%q", sender.Text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"content":[{"type":"text","text":"reply"},{"type":"image","data":"cG5n"},{"type":"emoji","data":"ZW1vamk="}]}]}`))
	}))
	defer server.Close()

	plugin := newPawzoChatPlugin()
	outputs, noReply, err := plugin.requestReply(Config{
		BaseURL: server.URL, Token: "token", HTTPTimeoutSeconds: 2,
	}, "persona", incomingMessage{
		SessionKey: "private:wxid", SpeakerName: "好友昵称", Text: "hello",
	})
	if err != nil {
		t.Fatalf("requestReply: %v", err)
	}
	if noReply {
		t.Fatal("normal bridge response was treated as no_reply")
	}
	if len(outputs) != 3 || outputs[0].Kind != "text" || outputs[0].Text != "reply" {
		t.Fatalf("outputs=%#v", outputs)
	}
	if outputs[1].Kind != "image" || string(outputs[1].Data) != "png" {
		t.Fatalf("image output=%#v", outputs[1])
	}
	if outputs[2].Kind != "emoji" || string(outputs[2].Data) != "emoji" {
		t.Fatalf("emoji output=%#v", outputs[2])
	}
}

func TestRequestReplyReturnsBridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Persona not found"}`))
	}))
	defer server.Close()

	plugin := newPawzoChatPlugin()
	_, _, err := plugin.requestReply(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 2,
	}, "missing", incomingMessage{SessionKey: "private:wxid", Text: "hello"})
	if err == nil || err.Error() != "PawzoChat request failed: Persona not found" {
		t.Fatalf("error=%v", err)
	}
}

func TestRequestReplyAcceptsExplicitNoReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"outcome":"no_reply","messages":[]}`))
	}))
	defer server.Close()

	plugin := newPawzoChatPlugin()
	outputs, noReply, err := plugin.requestReply(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 2,
	}, "persona", incomingMessage{SessionKey: "private:wxid", Text: "hello"})

	if err != nil || !noReply || len(outputs) != 0 {
		t.Fatalf("outputs=%#v noReply=%v err=%v", outputs, noReply, err)
	}
}

func TestRequestReplyDropsLeakedNoReplyMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"outcome":"replied","messages":[{"content":[{"type":"text","text":"reply"}]},{"content":[{"type":"text","text":"[[PAWZOCHAT_NO_REPLY]]"}]}]}`))
	}))
	defer server.Close()

	plugin := newPawzoChatPlugin()
	outputs, noReply, err := plugin.requestReply(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 2,
	}, "persona", incomingMessage{SessionKey: "private:wxid", Text: "hello"})

	if err != nil || noReply || len(outputs) != 1 || outputs[0].Text != "reply" {
		t.Fatalf("outputs=%#v noReply=%v err=%v", outputs, noReply, err)
	}
}

func TestRequestReplyTreatsLeakedMarkerOnlyAsNoReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"outcome":"replied","messages":[{"content":[{"type":"text","text":"[[PAWZOCHAT_NO_REPLY]]"}]}]}`))
	}))
	defer server.Close()

	plugin := newPawzoChatPlugin()
	outputs, noReply, err := plugin.requestReply(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 2,
	}, "persona", incomingMessage{SessionKey: "private:wxid", Text: "hello"})

	if err != nil || !noReply || len(outputs) != 0 {
		t.Fatalf("outputs=%#v noReply=%v err=%v", outputs, noReply, err)
	}
}

func TestRequestReplyCancelsTimedOutServerRequest(t *testing.T) {
	requestID := make(chan string, 1)
	cancelledID := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			parts := strings.Split(r.URL.Path, "/")
			cancelledID <- parts[len(parts)-2]
			_, _ = w.Write([]byte(`{"cancelled":true}`))
			return
		}
		var request bridgeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("Decode: %v", err)
		}
		requestID <- request.RequestID
		<-r.Context().Done()
	}))
	defer server.Close()

	plugin := newPawzoChatPlugin()
	_, _, err := plugin.requestReply(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 1,
	}, "persona", incomingMessage{SessionKey: "private:wxid", Text: "hello"})
	if err == nil {
		t.Fatal("request did not time out")
	}
	want := <-requestID
	select {
	case got := <-cancelledID:
		if got != want {
			t.Fatalf("cancelled request=%q want=%q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel endpoint was not called")
	}
}

func TestNormalizeConfigAndRoute(t *testing.T) {
	config := normalizeConfigValue(Config{
		BaseURL: " http://localhost:62000/ ",
		Routes:  map[string]string{" private:wxid ": " persona ", "bad": ""},
	})
	plugin := newPawzoChatPlugin()
	if config.BaseURL != "http://localhost:62000" || config.HTTPTimeoutSeconds != 50 {
		t.Fatalf("config=%#v", config)
	}
	if actual := plugin.personaForSession(config, "private:wxid"); actual != "persona" {
		t.Fatalf("persona=%q", actual)
	}
}

func TestMissingMediaBecomesVisiblePlaceholder(t *testing.T) {
	outputs, err := (bridgeResponse{Messages: []bridgeMessage{{
		Content: []bridgeBlock{{Type: "image"}, {Type: "voice"}},
	}}}).outputs()
	if err != nil {
		t.Fatalf("outputs: %v", err)
	}
	if len(outputs) != 2 || outputs[0].Text != "[图片]" || outputs[1].Text != "[语音]" {
		t.Fatalf("outputs=%#v", outputs)
	}
}

func TestLimitOutboundTextKeepsThreeTextsAndIndependentMedia(t *testing.T) {
	outputs := []outbound{
		{Kind: "text", Text: "one"},
		{Kind: "image", Data: []byte("image")},
		{Kind: "text", Text: "two"},
		{Kind: "text", Text: "three"},
		{Kind: "emoji", Data: []byte("emoji")},
		{Kind: "text", Text: "four"},
	}

	limited := limitOutboundText(outputs)

	textCount := 0
	mediaCount := 0
	var lastText string
	for _, output := range limited {
		if output.Kind == "text" {
			textCount++
			lastText = output.Text
		} else {
			mediaCount++
		}
	}
	if textCount != 3 || mediaCount != 2 || !strings.Contains(lastText, "four") {
		t.Fatalf("limited=%#v", limited)
	}
}

func TestPrepareOutboundTextStripsMarkdownAndCoalescesAdjacentText(t *testing.T) {
	outputs := []outbound{
		{Kind: "text", Text: "来了老弟，今天的新闻整理："},
		{Kind: "text", Text: "🔥 **国内要闻**"},
		{Kind: "text", Text: "- 暴雨橙色预警\n### 国际\n[新闻原文](https://example.com/news)"},
		{Kind: "emoji", Data: []byte("emoji")},
		{Kind: "text", Text: "> `补充说明`"},
	}

	prepared := prepareOutboundText(outputs)

	if len(prepared) != 3 {
		t.Fatalf("prepared=%#v", prepared)
	}
	want := "来了老弟，今天的新闻整理：\n🔥 国内要闻\n• 暴雨橙色预警\n国际\n新闻原文 (https://example.com/news)"
	if prepared[0].Kind != "text" || prepared[0].Text != want {
		t.Fatalf("text=%q want=%q", prepared[0].Text, want)
	}
	if prepared[1].Kind != "emoji" || prepared[2].Text != "补充说明" {
		t.Fatalf("prepared=%#v", prepared)
	}
}

func TestMarkdownToPlainTextPreservesPlainAsterisksAndFencedCode(t *testing.T) {
	input := "2 * 3 * 4\n```go\nfmt.Println(\"ok\")\n```\n---"
	want := "2 * 3 * 4\nfmt.Println(\"ok\")"
	if got := markdownToPlainText(input); got != want {
		t.Fatalf("markdownToPlainText()=%q want=%q", got, want)
	}
}
