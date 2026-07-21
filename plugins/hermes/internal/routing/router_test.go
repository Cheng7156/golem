package routing

import (
	"context"
	"testing"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

type fixedSocialDecider struct {
	route domain.Route
	calls int
}

func (d *fixedSocialDecider) Decide(
	context.Context,
	domain.InboxEvent,
	domain.InboundMessage,
) (domain.Route, string, error) {
	d.calls++
	return d.route, "test social decision", nil
}

type fixedContextReader struct {
	newer bool
}

func validHybridConfig() config.Config {
	value := config.Default()
	value.Routing.SocialMode = "hybrid"
	value.Routing.DecisionBaseURL = "https://example.com/v1"
	value.Routing.DecisionModel = "test-model"
	return value
}

func (r fixedContextReader) ListRecentInboundContext(
	context.Context,
	string,
	int64,
	int,
) ([]domain.ContextMessage, error) {
	return nil, nil
}

func (r fixedContextReader) HasNewerInboundFromSpeaker(
	context.Context,
	string,
	int64,
	string,
	time.Time,
) (bool, error) {
	return r.newer, nil
}

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

func TestAgentModeStillRoutesMessageAddressedToOtherMember(t *testing.T) {
	manager, _ := config.NewManager(config.Default())
	router, _ := NewRulesRouter(manager.Current, nil)
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "event"}, domain.InboundMessage{
		Text:            "@火 你在做什么",
		SpeakerID:       "member",
		IsChatroom:      true,
		MentionedOthers: true,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteChat || decision.Lane != domain.LaneInteractive {
		t.Fatalf("agent mode lost autonomous group participation: %#v", decision)
	}
}

func TestAgentModeKeepsLongTasksInInteractiveLane(t *testing.T) {
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router, err := NewRulesRouter(manager.Current, nil)
	if err != nil {
		t.Fatalf("NewRulesRouter: %v", err)
	}
	for _, text := range []string{"搜索一下最近的消息", "创建一个黑丝视频任务"} {
		decision, routeErr := router.Route(
			context.Background(),
			domain.InboxEvent{ID: "event-" + text},
			domain.InboundMessage{Text: text, SpeakerID: "owner", IsChatroom: true},
		)
		if routeErr != nil {
			t.Fatalf("Route(%q): %v", text, routeErr)
		}
		if decision.Route != domain.RouteChat || decision.Lane != domain.LaneInteractive {
			t.Fatalf("long task %q left interactive conversation: %#v", text, decision)
		}
	}
}

func TestAgentModeHonorsSampleRate(t *testing.T) {
	value := config.Default()
	value.Routing.SampleRate = 0
	manager, err := config.NewManager(value)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router, err := NewRulesRouter(manager.Current, nil)
	if err != nil {
		t.Fatalf("NewRulesRouter: %v", err)
	}
	decision, err := router.Route(context.Background(), domain.InboxEvent{ID: "ambient"}, domain.InboundMessage{
		Text: "ambient conversation", SpeakerID: "member", IsChatroom: true,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteObserve {
		t.Fatalf("sample_rate=0 decision=%#v", decision)
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

func TestMentionsModeRoutesOnlyMessagesAddressingHermes(t *testing.T) {
	value := config.Default()
	value.Routing.SocialMode = "mentions"
	manager, err := config.NewManager(value)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router, _ := NewRulesRouter(manager.Current, nil)
	tests := []struct {
		name    string
		message domain.InboundMessage
		want    domain.Route
	}{
		{
			name: "ambient group message",
			message: domain.InboundMessage{
				Text: "ambient conversation", SpeakerID: "member", IsChatroom: true,
			},
			want: domain.RouteObserve,
		},
		{
			name: "message mentioning another participant",
			message: domain.InboundMessage{
				Text: "@火 你在做什么", SpeakerID: "member", IsChatroom: true, MentionedOthers: true,
			},
			want: domain.RouteObserve,
		},
		{
			name: "message mentioning Hermes",
			message: domain.InboundMessage{
				Text: "@hermes 看一下", SpeakerID: "member", IsChatroom: true, Mentioned: true,
			},
			want: domain.RouteChat,
		},
		{
			name: "message quoting Hermes",
			message: domain.InboundMessage{
				Text: "继续", SpeakerID: "member", IsChatroom: true, Quoted: true,
			},
			want: domain.RouteChat,
		},
		{
			name: "private message",
			message: domain.InboundMessage{
				Text: "hello", SpeakerID: "member",
			},
			want: domain.RouteChat,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, routeErr := router.Route(
				context.Background(), domain.InboxEvent{ID: "event-" + test.name}, test.message,
			)
			if routeErr != nil {
				t.Fatalf("Route: %v", routeErr)
			}
			if decision.Route != test.want {
				t.Fatalf("decision=%#v want route=%s", decision, test.want)
			}
		})
	}
}

func TestHybridWithoutDeciderFailsClosedToObserve(t *testing.T) {
	manager, err := config.NewManager(validHybridConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
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

func TestHybridRoutesOwnerAmbientDirectlyAfterFastFilters(t *testing.T) {
	value := validHybridConfig()
	manager, _ := config.NewManager(value)
	decider := &fixedSocialDecider{route: domain.RouteObserve}
	router, _ := NewRulesRouter(manager.Current, decider)
	decision, err := router.Route(context.Background(), domain.InboxEvent{
		ID: "owner-follow-up", SessionID: "chatroom:test",
		Binding: domain.ChannelBinding{Principal: domain.Principal{
			ID: "owner", Name: "Owner", IsOwner: true,
		}},
	}, domain.InboundMessage{
		Text: "我还以为你出问题了", SpeakerID: "owner", SpeakerName: "Owner", IsChatroom: true,
		OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteChat || decision.Lane != domain.LaneInteractive {
		t.Fatalf("owner ambient decision=%#v", decision)
	}
	if decider.calls != 0 {
		t.Fatal("owner ambient message reached SocialDecider")
	}
}

func TestHybridStillFastFiltersOwnerAmbientRisks(t *testing.T) {
	value := validHybridConfig()
	manager, _ := config.NewManager(value)
	decider := &fixedSocialDecider{route: domain.RouteChat}
	tests := []struct {
		name    string
		reader  AmbientContextReader
		message domain.InboundMessage
	}{
		{
			name: "mentioning another participant",
			message: domain.InboundMessage{
				Text: "@火 你看一下", SpeakerID: "owner", IsChatroom: true,
				MentionedOthers: true, OccurredAt: time.Now(),
			},
		},
		{
			name: "standalone sticker",
			message: domain.InboundMessage{
				Text: "[sticker]", SpeakerID: "owner", IsChatroom: true,
				OccurredAt: time.Now(), Media: []domain.InboundMedia{{Kind: "emoji"}},
			},
		},
		{
			name:   "earlier fragment",
			reader: fixedContextReader{newer: true},
			message: domain.InboundMessage{
				Text: "你等下", SpeakerID: "owner", IsChatroom: true,
				OccurredAt: time.Now().Add(-2 * time.Second),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var router *RulesRouter
			if test.reader == nil {
				router, _ = NewRulesRouter(manager.Current, decider)
			} else {
				router, _ = NewRulesRouter(manager.Current, decider, test.reader)
			}
			decision, err := router.Route(context.Background(), domain.InboxEvent{
				ID: "owner-risk-" + test.name, SessionID: "chatroom:test", AcceptSeq: 10,
				Binding: domain.ChannelBinding{Principal: domain.Principal{ID: "owner", IsOwner: true}},
			}, test.message)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if decision.Route != domain.RouteObserve {
				t.Fatalf("owner risk decision=%#v", decision)
			}
		})
	}
}

func TestHybridFastFiltersHighRiskAmbientMessages(t *testing.T) {
	value := validHybridConfig()
	value.Routing.AutomatedSpeakerNames = []string{"ovo"}
	manager, _ := config.NewManager(value)
	decider := &fixedSocialDecider{route: domain.RouteChat}
	router, _ := NewRulesRouter(manager.Current, decider)
	tests := []struct {
		name      string
		principal domain.Principal
		message   domain.InboundMessage
	}{
		{
			name: "addressed to another participant",
			message: domain.InboundMessage{
				Text: "@火 你看一下", SpeakerID: "member", IsChatroom: true,
				MentionedOthers: true, OccurredAt: time.Now(),
			},
		},
		{
			name:      "configured bot speaker",
			principal: domain.Principal{ID: "bot-ovo", Name: "ovo"},
			message: domain.InboundMessage{
				Text: "主人说躺平", SpeakerID: "bot-ovo", SpeakerName: "ovo",
				IsChatroom: true, OccurredAt: time.Now(),
			},
		},
		{
			name: "standalone sticker",
			message: domain.InboundMessage{
				Text: "[sticker]", SpeakerID: "member", IsChatroom: true,
				OccurredAt: time.Now(), Media: []domain.InboundMedia{{Kind: "emoji"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := decider.calls
			decision, err := router.Route(context.Background(), domain.InboxEvent{
				ID: "event-" + test.name, SessionID: "chatroom:test",
				Binding: domain.ChannelBinding{Principal: test.principal},
			}, test.message)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if decision.Route != domain.RouteObserve {
				t.Fatalf("decision=%#v", decision)
			}
			if decider.calls != before {
				t.Fatal("fast-observed message reached SocialDecider")
			}
		})
	}
}

func TestHybridCoalescesNewerFragmentBeforeSocialDecision(t *testing.T) {
	value := validHybridConfig()
	manager, _ := config.NewManager(value)
	decider := &fixedSocialDecider{route: domain.RouteChat}
	router, _ := NewRulesRouter(manager.Current, decider, fixedContextReader{newer: true})
	decision, err := router.Route(context.Background(), domain.InboxEvent{
		ID: "fragment", SessionID: "chatroom:test", AcceptSeq: 10,
	}, domain.InboundMessage{
		Text: "爆了 271.\n\n6", SpeakerID: "member", IsChatroom: true,
		OccurredAt: time.Now().Add(-2 * time.Second),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteObserve || decider.calls != 0 {
		t.Fatalf("decision=%#v decider_calls=%d", decision, decider.calls)
	}
}

func TestHybridAppliesAmbientCooldownAndWindowLimit(t *testing.T) {
	value := validHybridConfig()
	value.Routing.AmbientCooldownSeconds = 10
	value.Routing.AmbientWindowSeconds = 60
	value.Routing.AmbientMaxReplies = 2
	manager, _ := config.NewManager(value)
	decider := &fixedSocialDecider{route: domain.RouteChat}
	router, _ := NewRulesRouter(manager.Current, decider)
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	router.now = func() time.Time { return now }
	route := func(id, text string) domain.Route {
		decision, err := router.Route(context.Background(), domain.InboxEvent{
			ID: id, SessionID: "chatroom:test",
		}, domain.InboundMessage{Text: text, SpeakerID: "member", IsChatroom: true})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		return decision.Route
	}
	if got := route("one", "first"); got != domain.RouteChat {
		t.Fatalf("first route=%s", got)
	}
	now = now.Add(5 * time.Second)
	if got := route("two", "second"); got != domain.RouteObserve {
		t.Fatalf("cooldown route=%s", got)
	}
	now = now.Add(6 * time.Second)
	if got := route("three", "third"); got != domain.RouteChat {
		t.Fatalf("second allowed route=%s", got)
	}
	now = now.Add(11 * time.Second)
	if got := route("four", "fourth"); got != domain.RouteObserve {
		t.Fatalf("window-limit route=%s", got)
	}
}

func TestHybridConvertsSocialJobDecisionToInteractiveChat(t *testing.T) {
	manager, err := config.NewManager(validHybridConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	decider := &fixedSocialDecider{route: domain.RouteJob}
	router, err := NewRulesRouter(manager.Current, decider)
	if err != nil {
		t.Fatalf("NewRulesRouter: %v", err)
	}
	decision, err := router.Route(context.Background(), domain.InboxEvent{
		ID: "semantic-job", SessionID: "chatroom:test",
	}, domain.InboundMessage{Text: "帮忙查一下", SpeakerID: "member", IsChatroom: true})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if decision.Route != domain.RouteChat || decision.Lane != domain.LaneInteractive {
		t.Fatalf("social job escaped interactive conversation: %#v", decision)
	}
}
