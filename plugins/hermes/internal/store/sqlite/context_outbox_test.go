package sqlite_test

import (
	"context"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func TestObservationAckAcceptsInsertedAndDuplicateItems(t *testing.T) {
	for _, status := range []string{"committed", "duplicate"} {
		t.Run(status, func(t *testing.T) {
			value := openStore(t)
			ctx := context.Background()
			if _, inserted, err := value.AcceptInbox(ctx, inboxEvent("ack-"+status, 701)); err != nil || !inserted {
				t.Fatalf("AcceptInbox inserted=%v err=%v", inserted, err)
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
