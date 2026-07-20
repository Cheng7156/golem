package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
)

func TestExpiredVideoLeaseIsDeadLetteredWithoutRedelivery(t *testing.T) {
	value := openStore(t)
	now := time.Now()
	outboxID := createLeasedDirectVideo(t, value, now)

	_, err := value.LeaseNextOutbox(context.Background(), now.Add(2*time.Second), time.Minute)
	if !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("LeaseNextOutbox error=%v, want ErrNotFound", err)
	}
	assertSuppressedVideo(t, value, outboxID, "lease expired")
}

func TestRecoveryDeadLettersInterruptedVideo(t *testing.T) {
	value := openStore(t)
	now := time.Now()
	outboxID := createLeasedDirectVideo(t, value, now)

	result, err := value.Recover(context.Background(), now.Add(time.Second))
	if err != nil || result.OutboxRecovered != 1 {
		t.Fatalf("Recover result=%#v error=%v", result, err)
	}
	assertSuppressedVideo(t, value, outboxID, "plugin restarted")
}

func createLeasedDirectVideo(t *testing.T, value *sqlite.Store, now time.Time) string {
	t.Helper()
	fixture := createRunningRun(t, value, "video-safety")
	createTestVideoObjects(t, value, now)
	ticket, err := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(fixture, "video-safety"),
	)
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	commit := directEmojiCommit(t, ticket, "call-video-safety")
	commit.Output = asyncOutput(t, "video", domain.VideoOutput{
		ObjectID: "media_video", ThumbObjectID: "media_thumb", Duration: 3,
	})
	queued, err := value.CommitAsyncDirectOutput(context.Background(), commit)
	if err != nil || !queued.Queued {
		t.Fatalf("CommitAsyncDirectOutput result=%#v error=%v", queued, err)
	}
	leased, err := value.LeaseNextOutbox(context.Background(), now, time.Second)
	if err != nil || leased.ID != queued.OutboxID || leased.Attempt != 1 {
		t.Fatalf("LeaseNextOutbox item=%#v error=%v", leased, err)
	}
	return queued.OutboxID
}

func assertSuppressedVideo(t *testing.T, value *sqlite.Store, outboxID, messagePart string) {
	t.Helper()
	stored, err := value.GetOutbox(context.Background(), outboxID)
	if err != nil || stored.State != domain.OutboxDeadLetter ||
		!strings.Contains(stored.LastError, messagePart) {
		t.Fatalf("outbox=%#v error=%v", stored, err)
	}
	attempts, err := value.ListDeliveryAttempts(context.Background(), outboxID)
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != "ambiguous" {
		t.Fatalf("attempts=%#v error=%v", attempts, err)
	}
}
