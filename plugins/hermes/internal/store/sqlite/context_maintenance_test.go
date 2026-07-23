package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func TestRepairObservationConversationRequeuesEarliestTerminalRangeInOrder(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	conversationID := acceptObservationConversation(t, value, "repair", 3)
	batch, err := value.LeaseNextObservationBatch(ctx, time.Now().Add(time.Second), time.Minute, 2)
	if err != nil {
		t.Fatalf("LeaseNextObservationBatch: %v", err)
	}
	if err := value.MarkObservationBatchTerminal(ctx, batch, domain.ContextConflict, "ack conflict"); err != nil {
		t.Fatalf("MarkObservationBatchTerminal: %v", err)
	}
	if ready, err := value.ObservationContextReady(ctx, conversationID, 3); ready || !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("ready=%v err=%v, want terminal conflict", ready, err)
	}

	repaired, err := value.RepairObservationConversation(ctx, conversationID, time.Now())
	if err != nil || repaired != 2 {
		t.Fatalf("RepairObservationConversation=%d, %v; want 2", repaired, err)
	}
	retry, err := value.LeaseNextObservationBatch(ctx, time.Now().Add(time.Second), time.Minute, 8)
	if err != nil {
		t.Fatalf("lease repaired batch: %v", err)
	}
	if retry.FirstConversationSeq != 1 || retry.LastConversationSeq != 3 {
		t.Fatalf("repaired batch range=%d..%d, want ordered 1..3", retry.FirstConversationSeq, retry.LastConversationSeq)
	}
	if retry.Observations[0].PayloadHash != batch.Observations[0].PayloadHash ||
		retry.Observations[1].PayloadHash != batch.Observations[1].PayloadHash {
		t.Fatal("repair changed immutable observation payload hashes")
	}
}

func TestRepairObservationConversationDoesNotSkipNonTerminalHead(t *testing.T) {
	value := openStore(t)
	conversationID := acceptObservationConversation(t, value, "pending-head", 1)
	if _, err := value.RepairObservationConversation(context.Background(), conversationID, time.Now()); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("RepairObservationConversation error=%v, want conflict", err)
	}
}

func TestPruneAckedObservationsPreservesConversationSequenceWatermark(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	conversationID := acceptObservationConversation(t, value, "prune", 3)
	batch, err := value.LeaseNextObservationBatch(ctx, time.Now().Add(time.Second), time.Minute, 8)
	if err != nil {
		t.Fatalf("LeaseNextObservationBatch: %v", err)
	}
	ack := domain.ObservationAck{RequestID: batch.RequestID, BatchID: batch.BatchID,
		ConversationID: conversationID, BatchHash: batch.BatchHash, Status: "committed",
		DurableThroughConversationSeq: batch.LastConversationSeq}
	if err := value.MarkObservationBatchAcked(ctx, batch, ack); err != nil {
		t.Fatalf("MarkObservationBatchAcked: %v", err)
	}
	pruned, err := value.PruneAckedObservations(ctx, time.Now().Add(time.Hour), 1000)
	if err != nil || pruned != 2 {
		t.Fatalf("PruneAckedObservations=%d, %v; want 2", pruned, err)
	}

	event := observationEvent("prune", 4)
	stored, inserted, err := value.AcceptInbox(ctx, event)
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox after prune inserted=%v err=%v", inserted, err)
	}
	item, err := value.GetContextOutboxByEvent(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetContextOutboxByEvent: %v", err)
	}
	if item.ConversationID != conversationID || item.ConversationSeq != 4 {
		t.Fatalf("new observation conversation=%q seq=%d, want %q seq=4", item.ConversationID,
			item.ConversationSeq, conversationID)
	}
}

func TestObservationMaintenanceStatusReportsTerminalRows(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	conversationID := acceptObservationConversation(t, value, "status", 1)
	batch, err := value.LeaseNextObservationBatch(ctx, time.Now().Add(time.Second), time.Minute, 8)
	if err != nil {
		t.Fatalf("LeaseNextObservationBatch: %v", err)
	}
	if err := value.MarkObservationBatchTerminal(ctx, batch, domain.ContextDeadLetter, "retry exhausted"); err != nil {
		t.Fatalf("MarkObservationBatchTerminal: %v", err)
	}
	status, err := value.ObservationMaintenanceStatus(ctx)
	if err != nil {
		t.Fatalf("ObservationMaintenanceStatus: %v", err)
	}
	if status.DeadLetter != 1 || status.BlockedConversations != 1 || status.OldestTerminalUpdated.IsZero() {
		t.Fatalf("status=%#v for conversation %s", status, conversationID)
	}
}

func acceptObservationConversation(t *testing.T, value interface {
	AcceptInbox(context.Context, domain.InboxEvent) (domain.InboxEvent, bool, error)
	MaterializeTurn(context.Context, string, int) (domain.Turn, error)
	RouteTurn(context.Context, string, domain.Route, domain.Lane, time.Time) (domain.Turn, *domain.Run, error)
}, suffix string, count int) string {
	t.Helper()
	var conversationID string
	for index := 1; index <= count; index++ {
		event := observationEvent(suffix, index)
		stored, inserted, err := value.AcceptInbox(context.Background(), event)
		if err != nil || !inserted {
			t.Fatalf("AcceptInbox %d inserted=%v err=%v", index, inserted, err)
		}
		conversationID = domain.StableConversationID(stored.Binding)
		turn, err := value.MaterializeTurn(context.Background(), stored.ID, 10)
		if err != nil {
			t.Fatalf("MaterializeTurn %d: %v", index, err)
		}
		if _, _, err := value.RouteTurn(context.Background(), turn.ID, domain.RouteObserve, "", time.Time{}); err != nil {
			t.Fatalf("RouteTurn %d: %v", index, err)
		}
	}
	return conversationID
}

func observationEvent(suffix string, index int) domain.InboxEvent {
	event := inboxEvent(fmt.Sprintf("%s-%d", suffix, index), int64(800+index))
	event.SessionID = "chatroom:room-" + suffix
	event.Binding.SessionID = event.SessionID
	event.Binding.ReceiverID = "room-" + suffix
	return event
}
