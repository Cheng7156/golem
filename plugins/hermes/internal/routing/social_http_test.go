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
		_, _ = recorder.WriteString(`{"choices":[{"message":{"role":"assistant","content":"{\"disposition\":\"observe\",\"reason_code\":\"other_owner\",\"reason\":\"其他机器人的主人不是 ccff 的主人\",\"confidence\":0.98}"}}]}`)
		return recorder.Result(), nil
	})}
	decider, err := NewHTTPSocialDecider(HTTPSocialDeciderConfig{
		BaseURL: "https://decision.example/v1", APIKey: "test-api-key-123456",
		Model: "glm-test", ContextMessages: 10,
	}, nil, client)
	if err != nil {
		t.Fatalf("NewHTTPSocialDecider: %v", err)
	}
	decision, err := decider.Decide(context.Background(), domain.InboxEvent{
		Binding: domain.ChannelBinding{Principal: domain.Principal{ID: "bot", Name: "ovo"}},
	}, domain.InboundMessage{
		Text: "人家主人说躺平", SpeakerID: "bot", SpeakerName: "ovo", IsChatroom: true,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Route != domain.RouteObserve || decision.Disposition != DispositionObserve ||
		!strings.Contains(decision.Reason, "不是") {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestParseSocialDecisionRejectsUnknownDisposition(t *testing.T) {
	if _, err := parseSocialDecision(`{"disposition":"reply","reason_code":"bad","reason":"no","confidence":1}`); err == nil {
		t.Fatal("unknown disposition was accepted")
	}
}

func TestHTTPSocialDeciderLowConfidenceRespondFallsBackToObserve(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		_, _ = recorder.WriteString(`{"choices":[{"message":{"role":"assistant","content":"{\"disposition\":\"respond\",\"reason_code\":\"maybe_useful\",\"reason\":\"可能有帮助\",\"confidence\":0.41}"}}]}`)
		return recorder.Result(), nil
	})}
	decider, err := NewHTTPSocialDecider(HTTPSocialDeciderConfig{
		BaseURL: "https://decision.example/v1", APIKey: "test-api-key-123456",
		Model: "glm-test", MinConfidence: 0.72,
	}, nil, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := decider.Decide(context.Background(), domain.InboxEvent{}, domain.InboundMessage{Text: "maybe"})
	if err != nil || decision.Route != domain.RouteObserve || decision.Disposition != DispositionObserve ||
		!strings.Contains(decision.Reason, "低置信") {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}

func TestHTTPSocialDeciderReturnsIgnoreDisposition(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		_, _ = recorder.WriteString(`{"choices":[{"message":{"role":"assistant","content":"{\"disposition\":\"ignore\",\"reason_code\":\"noise\",\"reason\":\"自动播报\",\"confidence\":0.99}"}}]}`)
		return recorder.Result(), nil
	})}
	decider, err := NewHTTPSocialDecider(HTTPSocialDeciderConfig{
		BaseURL: "https://decision.example/v1", APIKey: "test-api-key-123456", Model: "glm-test",
	}, nil, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := decider.Decide(context.Background(), domain.InboxEvent{}, domain.InboundMessage{Text: "noise"})
	if err != nil || decision.Disposition != DispositionIgnore || decision.Route != domain.RouteObserve {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}
