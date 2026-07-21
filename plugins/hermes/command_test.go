package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/ingress"
	"golem_plugin_hermes/internal/routing"
	sqlitestore "golem_plugin_hermes/internal/store/sqlite"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/plugin"
)

const commandTestOwner = "wxid_owner"

func TestHermesCommandSchemaAndChineseHelp(t *testing.T) {
	p := newHermesPlugin()
	p.ownerID = commandTestOwner
	if commands := p.GetCommands(); len(commands) != 1 || commands[0] != "hermes" {
		t.Fatalf("GetCommands()=%v", commands)
	}
	schemas := p.GetCommandSchemas()
	if len(schemas) != 1 || schemas[0].GetMain() != "hermes" {
		t.Fatalf("GetCommandSchemas()=%v", schemas)
	}
	if !strings.Contains(schemas[0].GetDescription(), "仅机器人所有者") {
		t.Fatalf("schema description=%q", schemas[0].GetDescription())
	}

	result, err := p.OnCommand(privateHermesCommand("help"))
	if err != nil {
		t.Fatalf("OnCommand(help): %v", err)
	}
	for _, expected := range []string{"仅机器人所有者可用", "/hermes status", "/hermes personality", "/hermes observations repair current"} {
		if !strings.Contains(result, expected) {
			t.Errorf("help missing %q:\n%s", expected, result)
		}
	}
}

func TestOwnerCanRepairCurrentObservationConversationLocally(t *testing.T) {
	p, store := newCommandTestPlugin(t)
	ctx := context.Background()
	now := time.Now()
	event := domain.InboxEvent{ID: "event-observation-repair", DedupeKey: "wechat/message/observation-repair",
		Topic: "message.text", SessionID: "private:" + commandTestOwner, OccurredAt: now,
		Binding: domain.ChannelBinding{Channel: "wechat", SessionID: "private:" + commandTestOwner,
			ReceiverID: commandTestOwner, Principal: domain.Principal{ID: commandTestOwner, IsOwner: true}},
		Payload: json.RawMessage(`{"text":"hello"}`)}
	if _, inserted, err := store.AcceptInbox(ctx, event); err != nil || !inserted {
		t.Fatalf("AcceptInbox inserted=%v err=%v", inserted, err)
	}
	batch, err := store.LeaseNextObservationBatch(ctx, now.Add(time.Second), time.Minute, 8)
	if err != nil {
		t.Fatalf("LeaseNextObservationBatch: %v", err)
	}
	if err := store.MarkObservationBatchTerminal(ctx, batch, domain.ContextConflict, "conflict"); err != nil {
		t.Fatalf("MarkObservationBatchTerminal: %v", err)
	}

	result, err := p.OnCommand(privateHermesCommand("observations", "repair", "current"))
	if err != nil {
		t.Fatalf("OnCommand(observations repair): %v", err)
	}
	if !strings.Contains(result, "已校验并重排") || !strings.Contains(result, "未跳过序号") {
		t.Fatalf("repair result=%q", result)
	}
	status, err := store.ObservationMaintenanceStatus(ctx)
	if err != nil || status.Conflict != 0 || status.RetryWait != 1 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	events := acceptedCommandEvents(t, store)
	if len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("local maintenance unexpectedly forwarded an Inbox command: %#v", events)
	}
}

func TestNormalizeHermesCommandAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "status", input: "status", want: "/status"},
		{name: "leading slash", input: "/RESET", want: "/reset"},
		{name: "variadic personality", input: "personality technical", want: "/personality technical"},
		{name: "unsupported", input: "restart", wantErr: true},
		{name: "unexpected args", input: "status extra", wantErr: true},
		{name: "nested slash", input: "//status", wantErr: true},
		{name: "control character", input: "status\nrestart", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := normalizeHermesCommand(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("normalizeHermesCommand(%q)=%q, want error", test.input, actual)
				}
				return
			}
			if err != nil || actual != test.want {
				t.Fatalf("normalizeHermesCommand(%q)=%q, %v; want %q", test.input, actual, err, test.want)
			}
		})
	}
}

func TestHermesCommandOwnerAuthorization(t *testing.T) {
	t.Run("missing owner", func(t *testing.T) {
		p := newHermesPlugin()
		if _, err := p.OnCommand(privateHermesCommand("status")); err == nil || !strings.Contains(err.Error(), "未配置") {
			t.Fatalf("OnCommand() error=%v", err)
		}
	})
	t.Run("private non-owner", func(t *testing.T) {
		p := newHermesPlugin()
		p.ownerID = commandTestOwner
		command := privateHermesCommand("status")
		command.Sender.Username = "wxid_other"
		if _, err := p.OnCommand(command); err == nil || !strings.Contains(err.Error(), "仅允许机器人所有者") {
			t.Fatalf("OnCommand() error=%v", err)
		}
	})
}

func TestPrivateHermesCommandIsDurablyAccepted(t *testing.T) {
	p, store := newCommandTestPlugin(t)
	result, err := p.OnCommand(privateHermesCommand("status"))
	if err != nil {
		t.Fatalf("OnCommand(status): %v", err)
	}
	if result != "" {
		t.Fatalf("OnCommand(status)=%q, want empty direct reply", result)
	}

	events := acceptedCommandEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("accepted events=%d, want 1", len(events))
	}
	event := events[0]
	if event.SessionID != "private:"+commandTestOwner || event.Binding.ReceiverID != commandTestOwner {
		t.Fatalf("private event=%#v", event)
	}
	if !event.Binding.Principal.IsOwner || event.Binding.Principal.ID != commandTestOwner {
		t.Fatalf("private principal=%#v", event.Binding.Principal)
	}
	message := decodeCommandMessage(t, event)
	if message.HermesCommand != "/status" || message.Text != "/hermes status" {
		t.Fatalf("inbound message=%#v", message)
	}
}

func TestVariadicAndGroupHermesCommandsUseExpectedSession(t *testing.T) {
	p, store := newCommandTestPlugin(t)
	group := &plugin.Command{
		Raw:         "/hermes personality technical",
		Main:        "hermes",
		Positionals: []string{"personality", "technical"},
		Sender: &contact.Contact{
			Username: "room@chatroom",
			Nickname: "测试群",
			Type:     contact.ContactType_CONTACT_TYPE_CHATROOM,
		},
	}
	result, err := p.OnCommand(group)
	if err != nil {
		t.Fatalf("OnCommand(group): %v", err)
	}
	if result != "" {
		t.Fatalf("OnCommand(group)=%q, want empty direct reply", result)
	}

	events := acceptedCommandEvents(t, store)
	if len(events) != 1 {
		t.Fatalf("accepted events=%d, want 1", len(events))
	}
	event := events[0]
	if event.SessionID != "chatroom:room@chatroom" || event.Binding.ReceiverID != "room@chatroom" {
		t.Fatalf("group event=%#v", event)
	}
	if event.Binding.Principal.ID != commandTestOwner || !event.Binding.Principal.IsOwner {
		t.Fatalf("group principal=%#v", event.Binding.Principal)
	}
	message := decodeCommandMessage(t, event)
	if message.HermesCommand != "/personality technical" || !message.Explicit() {
		t.Fatalf("group message=%#v", message)
	}
}

func TestResetConfirmationCommandsShareWechatSession(t *testing.T) {
	p, store := newCommandTestPlugin(t)
	for _, command := range []string{"reset", "always"} {
		if result, err := p.OnCommand(privateHermesCommand(command)); err != nil || result != "" {
			t.Fatalf("OnCommand(%s)=%q, %v", command, result, err)
		}
	}
	events := acceptedCommandEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("accepted events=%d, want 2", len(events))
	}
	if events[0].SessionID != events[1].SessionID {
		t.Fatalf("sessions differ: %q != %q", events[0].SessionID, events[1].SessionID)
	}
	if first, second := decodeCommandMessage(t, events[0]), decodeCommandMessage(t, events[1]); first.HermesCommand != "/reset" || second.HermesCommand != "/always" {
		t.Fatalf("commands=%q, %q", first.HermesCommand, second.HermesCommand)
	}
}

func newCommandTestPlugin(t *testing.T) (*HermesPlugin, *sqlitestore.Store) {
	t.Helper()
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close store: %v", err)
		}
	})
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router, err := routing.NewRulesRouter(manager.Current, nil)
	if err != nil {
		t.Fatalf("NewRulesRouter: %v", err)
	}
	processor, err := ingress.NewProcessor(store, router, 0, nil)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	p := newHermesPlugin()
	p.store = store
	p.config = manager
	p.processor = processor
	p.ownerID = commandTestOwner
	return p, store
}

func privateHermesCommand(input ...string) *plugin.Command {
	return &plugin.Command{
		Raw:         "/hermes " + strings.Join(input, " "),
		Main:        "hermes",
		Positionals: input,
		Sender: &contact.Contact{
			Username: commandTestOwner,
			Nickname: "主人",
			Type:     contact.ContactType_CONTACT_TYPE_FRIEND,
		},
	}
}

func acceptedCommandEvents(t *testing.T, store *sqlitestore.Store) []domain.InboxEvent {
	t.Helper()
	events, err := store.ListInbox(context.Background(), domain.InboxAccepted, 20)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	return events
}

func decodeCommandMessage(t *testing.T, event domain.InboxEvent) domain.InboundMessage {
	t.Helper()
	var value domain.InboundMessage
	if err := json.Unmarshal(event.Payload, &value); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	return value
}
