package routing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golem_plugin_hermes/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPSocialDeciderSendsVerifiedRolesAndParsesStrictJSON(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://decision.example/v1/chat/completions" {
			t.Fatalf("URL=%s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer test-api-key-123456" {
			t.Fatalf("authorization=%q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		var completion socialCompletionRequest
		if err := json.Unmarshal(body, &completion); err != nil {
			t.Fatalf("request JSON: %v", err)
		}
		if len(completion.Messages) != 2 || !strings.Contains(completion.Messages[0].Content, "participant_not_owner") {
			t.Fatalf("messages=%#v", completion.Messages)
		}
		if completion.Thinking.Type != "disabled" {
			t.Fatalf("thinking=%#v", completion.Thinking)
		}
		if !strings.Contains(completion.Messages[1].Content, `"sender_role":"participant_not_owner"`) ||
			!strings.Contains(completion.Messages[1].Content, `"addressing":"none"`) {
			t.Fatalf("decision input=%s", completion.Messages[1].Content)
		}
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(http.StatusOK)
		_, _ = recorder.WriteString(`{"choices":[{"message":{"role":"assistant","content":"{\"action\":\"observe\",\"reason\":\"其他机器人的主人不是 ccff 的主人\"}"}}]}`)
		return recorder.Result(), nil
	})}
	decider, err := NewHTTPSocialDecider(HTTPSocialDeciderConfig{
		BaseURL: "https://decision.example/v1", APIKey: "test-api-key-123456",
		Model: "glm-test", ContextMessages: 10,
	}, nil, client)
	if err != nil {
		t.Fatalf("NewHTTPSocialDecider: %v", err)
	}
	route, reason, err := decider.Decide(context.Background(), domain.InboxEvent{
		Binding: domain.ChannelBinding{Principal: domain.Principal{ID: "bot", Name: "ovo"}},
	}, domain.InboundMessage{
		Text: "人家主人说躺平", SpeakerID: "bot", SpeakerName: "ovo", IsChatroom: true,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if route != domain.RouteObserve || !strings.Contains(reason, "不是") {
		t.Fatalf("route=%s reason=%q", route, reason)
	}
}

func TestParseSocialDecisionRejectsUnknownAction(t *testing.T) {
	if _, err := parseSocialDecision(`{"action":"reply","reason":"no"}`); err == nil {
		t.Fatal("unknown action was accepted")
	}
}
