package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func TestObservationAckAcceptsInsertedAndDuplicateItems(t *testing.T) {
	for _, status := range []string{"committed", "duplicate"} {
		t.Run(status, func(t *testing.T) {
			value := openStore(t)
			ctx := context.Background()
			event, inserted, err := value.AcceptInbox(ctx, inboxEvent("ack-"+status, 701))
			if err != nil || !inserted {
				t.Fatalf("AcceptInbox inserted=%v err=%v", inserted, err)
			}
			turn, err := value.MaterializeTurn(ctx, event.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := value.RouteTurn(ctx, turn.ID, domain.RouteObserve, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			batch, err := value.LeaseNextObservationBatch(ctx, time.Now().Add(time.Second), time.Minute, 8)
			if err != nil {
				t.Fatalf("LeaseNextObservationBatch: %v", err)
			}
			ack := domain.ObservationAck{RequestID: batch.RequestID, BatchID: batch.BatchID,
				ConversationID: batch.ConversationID, BatchHash: batch.BatchHash, Status: "committed",
				DurableThroughConversationSeq: batch.LastConversationSeq,
				Items:                         []domain.ObservationAckItem{{ObservationID: batch.Observations[0].ObservationID, Status: status}}}
			if err := value.MarkObservationBatchAcked(ctx, batch, ack); err != nil {
				t.Fatalf("MarkObservationBatchAcked(%s): %v", status, err)
			}
			ready, err := value.ObservationContextReady(ctx, batch.ConversationID, batch.LastConversationSeq)
			if err != nil || !ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestObservationCannotLeaseBeforeRoutingDecision(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	event, inserted, err := value.AcceptInbox(ctx, inboxEvent("routing-gate", 702))
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox inserted=%v err=%v", inserted, err)
	}
	turn, err := value.MaterializeTurn(ctx, event.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.LeaseNextObservationBatch(ctx, time.Now(), time.Minute, 8); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("lease before route err=%v, want not found", err)
	}
	if _, _, err := value.RouteTurn(ctx, turn.ID, domain.RouteObserve, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := value.LeaseNextObservationBatch(ctx, time.Now(), time.Minute, 8); err != nil {
		t.Fatalf("lease after route: %v", err)
	}
}

func TestIgnoredRoutingMarksOrderedSkipAndAcceptsIgnoredAck(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	event, inserted, err := value.AcceptInbox(ctx, inboxEvent("ignored", 703))
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox inserted=%v err=%v", inserted, err)
	}
	turn, err := value.MaterializeTurn(ctx, event.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := value.RouteTurnWithContextDisposition(
		ctx, turn.ID, domain.RouteObserve, "", time.Time{}, true,
	); err != nil {
		t.Fatal(err)
	}
	batch, err := value.LeaseNextObservationBatch(ctx, time.Now(), time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	if got := batch.Observations[0].TranscriptDisposition; got != "ignore" {
		t.Fatalf("transcript disposition=%q", got)
	}
	ack := domain.ObservationAck{RequestID: batch.RequestID, BatchID: batch.BatchID,
		ConversationID: batch.ConversationID, BatchHash: batch.BatchHash, Status: "committed",
		DurableThroughConversationSeq: batch.LastConversationSeq,
		Items:                         []domain.ObservationAckItem{{ObservationID: batch.Observations[0].ObservationID, Status: "ignored"}}}
	if err := value.MarkObservationBatchAcked(ctx, batch, ack); err != nil {
		t.Fatalf("MarkObservationBatchAcked: %v", err)
	}
	if ready, err := value.ObservationContextReady(ctx, batch.ConversationID, batch.LastConversationSeq); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
}
