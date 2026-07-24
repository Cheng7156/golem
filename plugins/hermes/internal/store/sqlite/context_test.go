package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/store/sqlite"
)

func TestRecentInboundContextAndNewerSpeakerLookup(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	first := contextInbox(t, "context-first", "member", "第一段", base)
	second := contextInbox(t, "context-second", "member", "第二段", base.Add(300*time.Millisecond))
	third := contextInbox(t, "context-third", "other", "插话", base.Add(500*time.Millisecond))
	for _, event := range []domain.InboxEvent{first, second, third} {
		stored, _, err := store.AcceptInbox(ctx, event)
		if err != nil {
			t.Fatalf("AcceptInbox: %v", err)
		}
		turn, err := store.MaterializeTurn(ctx, stored.ID, 10)
		if err != nil {
			t.Fatalf("MaterializeTurn: %v", err)
		}
		if _, _, err := store.RouteTurn(ctx, turn.ID, domain.RouteObserve, "", time.Time{}); err != nil {
			t.Fatalf("RouteTurn: %v", err)
		}
	}
	last, err := store.GetInbox(ctx, third.ID)
	if err != nil {
		t.Fatalf("GetInbox: %v", err)
	}
	values, err := store.ListRecentInboundContext(ctx, last.SessionID, last.AcceptSeq+1, 8)
	if err != nil {
		t.Fatalf("ListRecentInboundContext: %v", err)
	}
	if len(values) != 3 || values[0].Message.Text != "第一段" || values[2].Message.Text != "插话" {
		t.Fatalf("unexpected context=%#v", values)
	}
	if values[0].EventID != first.ID || values[0].PlatformMessageID != "" {
		t.Fatalf("first identity=%#v", values[0])
	}
	firstStored, err := store.GetInbox(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetInbox first: %v", err)
	}
	found, err := store.HasNewerInboundFromSpeaker(
		ctx, firstStored.SessionID, firstStored.AcceptSeq, "member", base.Add(time.Second),
	)
	if err != nil || !found {
		t.Fatalf("HasNewerInboundFromSpeaker found=%v err=%v", found, err)
	}
}

func contextInbox(t *testing.T, id, speakerID, text string, occurredAt time.Time) domain.InboxEvent {
	t.Helper()
	message := domain.InboundMessage{
		Text: text, IsChatroom: true, SpeakerID: speakerID, OccurredAt: occurredAt,
	}
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return domain.InboxEvent{
		ID: id, DedupeKey: id, SessionID: "chatroom:context", Topic: "text",
		OccurredAt: occurredAt, AcceptedAt: occurredAt,
		Binding: domain.ChannelBinding{
			Channel: "wechat", SessionID: "chatroom:context", ReceiverID: "chatroom",
			Principal: domain.Principal{ID: speakerID, Name: speakerID},
		},
		Payload: payload,
	}
}
