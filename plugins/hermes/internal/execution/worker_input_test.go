package execution

import (
	"strings"
	"testing"

	"golem_plugin_hermes/internal/domain"
)

func TestFormatAgentInputBridgesRelaySessionReset(t *testing.T) {
	message := domain.InboundMessage{Text: "hermes:new", SpeakerID: "user-1"}
	if actual := formatAgentInput("relay", message); actual != "/new" {
		t.Fatalf("formatAgentInput relay command=%q, want /new", actual)
	}

	message.Text = " HERMES:RESET "
	if actual := formatAgentInput("RELAY", message); actual != "/reset" {
		t.Fatalf("formatAgentInput relay command=%q, want /reset", actual)
	}
}

func TestFormatAgentInputUsesTrustedRelayCommandOnlyAtRelayBoundary(t *testing.T) {
	message := domain.InboundMessage{
		Text:          "/hermes cancel",
		HermesCommand: "/cancel",
		SpeakerID:     "owner",
	}
	if actual := formatAgentInput("relay", message); actual != "/cancel" {
		t.Fatalf("relay trusted command=%q, want /cancel", actual)
	}
	if actual := formatAgentInput("http", message); !strings.Contains(actual, "message: /hermes cancel") {
		t.Fatalf("http trusted command was incorrectly unwrapped: %q", actual)
	}
}

func TestFormatAgentInputDoesNotRewriteOrdinaryOrHTTPInput(t *testing.T) {
	tests := []struct {
		mode string
		text string
	}{
		{mode: "relay", text: "please explain hermes:new"},
		{mode: "relay", text: "/new"},
		{mode: "http", text: "hermes:new"},
	}
	for _, test := range tests {
		actual := formatAgentInput(test.mode, domain.InboundMessage{Text: test.text, SpeakerID: "user-1"})
		if !strings.Contains(actual, "message: "+strings.TrimSpace(test.text)) {
			t.Errorf("formatAgentInput(%q, %q)=%q", test.mode, test.text, actual)
		}
	}
}

func TestFormatAgentInputMarksGroupEngagementContext(t *testing.T) {
	ambient := formatAgentInput("relay", domain.InboundMessage{
		Text: "ambient", IsChatroom: true, SpeakerID: "user-1",
	})
	if !strings.HasPrefix(ambient, "[group ambient]") {
		t.Fatalf("ambient group input=%q", ambient)
	}
	addressed := formatAgentInput("relay", domain.InboundMessage{
		Text: "question", IsChatroom: true, Mentioned: true, SpeakerID: "user-1",
	})
	if !strings.HasPrefix(addressed, "[group addressed]") {
		t.Fatalf("addressed group input=%q", addressed)
	}
}
