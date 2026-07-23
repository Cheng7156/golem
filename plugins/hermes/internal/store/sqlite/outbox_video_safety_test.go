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

func TestExpiredVideoLeaseBeforeSendIsSafelyRequeued(t *testing.T) {
	value := openStore(t)
	now := time.Now()
	outboxID := createLeasedDirectVideo(t, value, now)

	leased, err := value.LeaseNextOutbox(context.Background(), now.Add(2*time.Second), time.Minute)
	if err != nil || leased.ID != outboxID || leased.Attempt != 2 {
		t.Fatalf("LeaseNextOutbox item=%#v error=%v", leased, err)
	}
	assertVideoAttempt(t, value, outboxID, domain.OutboxLeased, "not_started", "lease expired")
}

func TestRecoveryRequeuesVideoWhoseSendDidNotStart(t *testing.T) {
	value := openStore(t)
	now := time.Now()
	outboxID := createLeasedDirectVideo(t, value, now)

	result, err := value.Recover(context.Background(), now.Add(time.Second))
	if err != nil || result.OutboxRecovered != 1 {
		t.Fatalf("Recover result=%#v error=%v", result, err)
	}
	assertVideoAttempt(t, value, outboxID, domain.OutboxRetryWait, "not_started", "plugin restarted")
}

func TestExpiredVideoSendingLeaseBecomesAmbiguousWithoutRedelivery(t *testing.T) {
	value := openStore(t)
	now := time.Now()
	outboxID := createLeasedDirectVideo(t, value, now)
	item, err := value.GetOutbox(context.Background(), outboxID)
	if err != nil || value.MarkOutboxSending(context.Background(), outboxID, item.LeaseToken) != nil {
		t.Fatalf("mark sending item=%#v err=%v", item, err)
	}
	if _, err := value.LeaseNextOutbox(context.Background(), now.Add(2*time.Second), time.Minute); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("LeaseNextOutbox error=%v, want ErrNotFound", err)
	}
	assertVideoAttempt(t, value, outboxID, domain.OutboxAmbiguous, "ambiguous", "receipt is unknown")
}

func TestVideoJobBecomesDeliveredOnlyAfterWechatReceipt(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	now := time.Now()
	fixture := createRunningRun(t, value, "video-receipt")
	createTestVideoObjects(t, value, now)
	ticket, err := value.RegisterAsyncDelivery(ctx, asyncRegistration(fixture, "video-receipt"))
	if err != nil {
		t.Fatal(err)
	}
	job := domain.AsyncVideoJob{ID: "avjob-receipt", TicketHash: ticket.TicketHash,
		CandidateID: "candidate", InvocationID: "call-video-receipt", CreatedAt: now}
	if _, inserted, err := value.CreateAsyncVideoJob(ctx, job); err != nil || !inserted {
		t.Fatalf("CreateAsyncVideoJob inserted=%v err=%v", inserted, err)
	}
	leasedJob, err := value.LeaseAsyncVideoJob(ctx, job.ID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	commit := directEmojiCommit(t, ticket, job.InvocationID)
	commit.Output = asyncOutput(t, "video", domain.VideoOutput{
		ObjectID: "media_video", ThumbObjectID: "media_thumb", Duration: 3,
	})
	queued, err := value.CommitAsyncDirectOutput(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.FinishAsyncVideoJob(ctx, job.ID, leasedJob.LeaseToken, queued, ""); err != nil {
		t.Fatal(err)
	}
	waiting, _ := value.GetAsyncVideoJob(ctx, job.ID)
	if waiting.State != domain.AsyncVideoJobWaitingDelivery {
		t.Fatalf("job before receipt=%#v", waiting)
	}
	outbox, err := value.LeaseNextOutbox(ctx, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.MarkOutboxSending(ctx, outbox.ID, outbox.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := value.MarkOutboxSent(ctx, outbox.ID, outbox.LeaseToken, 9001, now); err != nil {
		t.Fatal(err)
	}
	delivered, _ := value.GetAsyncVideoJob(ctx, job.ID)
	if delivered.State != domain.AsyncVideoJobDelivered || delivered.Stage != "delivered" {
		t.Fatalf("job after receipt=%#v", delivered)
	}
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

func assertVideoAttempt(t *testing.T, value *sqlite.Store, outboxID string,
	state domain.OutboxState, outcome, messagePart string) {
	t.Helper()
	stored, err := value.GetOutbox(context.Background(), outboxID)
	if err != nil || stored.State != state ||
		!strings.Contains(stored.LastError, messagePart) {
		t.Fatalf("outbox=%#v error=%v", stored, err)
	}
	attempts, err := value.ListDeliveryAttempts(context.Background(), outboxID)
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != outcome {
		t.Fatalf("attempts=%#v error=%v", attempts, err)
	}
}
