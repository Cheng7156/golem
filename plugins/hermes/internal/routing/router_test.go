package routing

import (
	"context"
	"testing"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

func TestExplicitStopRoutesToControlLane(t *testing.T) {
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router, err := NewRulesRouter(manager.Current, nil)
	if err != nil {
		t.Fatalf("NewRulesRouter: %v", err)
	}
	decision, err := router.Route(context.Background(), domain.InboxEvent{}, domain.InboundMessage{
		Text:       "/stop",
		SpeakerID:  "owner",
		IsChatroom: false,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteControl || decision.Lane != domain.LaneControl {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestNonExplicitGroupStopIsNotControl(t *testing.T) {
	manager, _ := config.NewManager(config.Default())
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:       "/stop",
		SpeakerID:  "member",
		IsChatroom: true,
		Mentioned:  false,
		Quoted:     false,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route == domain.RouteControl {
		t.Fatalf("ordinary group command reached control lane: %#v", decision)
	}
	if decision.Route != domain.RouteChat || decision.Lane != domain.LaneInteractive {
		t.Fatalf("agent mode did not delegate ambient group message to Hermes: %#v", decision)
	}
}

func TestTrustedHermesCancelRoutesToInteractiveLane(t *testing.T) {
	manager, _ := config.NewManager(config.Default())
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:          "/hermes cancel",
		HermesCommand: "/cancel",
		SpeakerID:     "owner",
		IsChatroom:    true,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteChat || decision.Lane != domain.LaneInteractive {
		t.Fatalf("trusted Hermes command did not reach interactive lane: %#v", decision)
	}
}

func TestRelayAgentRoutesWithoutConnectorDeadline(t *testing.T) {
	manager, _ := config.NewManager(config.Default())
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:       "hello",
		SpeakerID:  "owner",
		IsChatroom: false,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !decision.Deadline.IsZero() {
		t.Fatalf("Relay route inherited connector deadline: %v", decision.Deadline)
	}
}

func TestHTTPAgentRetainsConnectorDeadline(t *testing.T) {
	value := config.Default()
	value.Agent.Mode = "http"
	manager, err := config.NewManager(value)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:       "hello",
		SpeakerID:  "owner",
		IsChatroom: false,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Deadline.IsZero() {
		t.Fatal("HTTP route lost its connector deadline")
	}
}

func TestRulesModeKeepsAmbientGroupMessageObserved(t *testing.T) {
	value := config.Default()
	value.Routing.SocialMode = "rules"
	manager, _ := config.NewManager(value)
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:       "ambient conversation",
		SpeakerID:  "member",
		IsChatroom: true,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteObserve {
		t.Fatalf("rules mode decision=%#v", decision)
	}
}

func TestHybridWithoutDeciderFailsClosedToObserve(t *testing.T) {
	value := config.Default()
	value.Routing.SocialMode = "hybrid"
	manager, _ := config.NewManager(value)
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:       "ambient conversation",
		SpeakerID:  "member",
		IsChatroom: true,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteObserve {
		t.Fatalf("hybrid fail-closed decision=%#v", decision)
	}
}
