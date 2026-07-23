package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func durableVideoJob(now time.Time) domain.AsyncVideoJob {
	return domain.AsyncVideoJob{
		ID: "avjob_durable", TicketHash: "ticket-hash",
		MediaURL: "https://cdn.example/video.mp4", Title: "sample",
		InvocationID: "call-video-1", CreatedAt: now,
	}
}

func TestAsyncVideoJobAndURLGrantSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hermes.db")
	now := time.Now().UTC().Truncate(time.Millisecond)
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	job := durableVideoJob(now)
	created, inserted, err := first.CreateAsyncVideoJob(ctx, job)
	if err != nil || !inserted || created.State != domain.AsyncVideoJobPending {
		t.Fatalf("CreateAsyncVideoJob job=%#v inserted=%v err=%v", created, inserted, err)
	}
	if err := first.RememberAsyncVideoURLs(
		ctx, job.TicketHash, []string{job.MediaURL}, now.Add(time.Hour),
	); err != nil {
		t.Fatalf("RememberAsyncVideoURLs: %v", err)
	}
	leased, err := first.LeaseAsyncVideoJob(ctx, job.ID, now, 30*time.Second)
	if err != nil || leased.State != domain.AsyncVideoJobRunning || leased.LeaseToken == "" {
		t.Fatalf("LeaseAsyncVideoJob job=%#v err=%v", leased, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	defer second.Close()
	stored, err := second.GetAsyncVideoJob(ctx, job.ID)
	if err != nil || stored.LeaseToken != leased.LeaseToken {
		t.Fatalf("GetAsyncVideoJob job=%#v err=%v", stored, err)
	}
	allowed, err := second.AsyncVideoURLAllowed(ctx, job.TicketHash, job.MediaURL, now)
	if err != nil || !allowed {
		t.Fatalf("AsyncVideoURLAllowed allowed=%v err=%v", allowed, err)
	}
	runnable, err := second.ListRunnableAsyncVideoJobs(ctx, now.Add(31*time.Second), 10)
	if err != nil || len(runnable) != 1 || runnable[0].ID != job.ID {
		t.Fatalf("ListRunnableAsyncVideoJobs jobs=%#v err=%v", runnable, err)
	}
	reclaimed, err := second.LeaseAsyncVideoJob(ctx, job.ID, now.Add(31*time.Second), time.Minute)
	if err != nil || reclaimed.LeaseToken == leased.LeaseToken || reclaimed.Attempt != 2 {
		t.Fatalf("reclaim job=%#v err=%v", reclaimed, err)
	}
	result := domain.AsyncDirectOutputResult{
		Queued: true, OutboxID: "outbox-video", Sequence: 7, DirectOutputCount: 1,
	}
	if err := second.FinishAsyncVideoJob(
		ctx, job.ID, reclaimed.LeaseToken, result, "",
	); err != nil {
		t.Fatalf("FinishAsyncVideoJob: %v", err)
	}
	finished, err := second.GetAsyncVideoJob(ctx, job.ID)
	if err != nil || finished.State != domain.AsyncVideoJobWaitingDelivery ||
		finished.Stage != "waiting_delivery" || finished.Result != result {
		t.Fatalf("finished job=%#v err=%v", finished, err)
	}
}

func TestAsyncVideoJobInvocationIsIdempotentAndInputBound(t *testing.T) {
	ctx := context.Background()
	value, err := Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer value.Close()
	job := durableVideoJob(time.Now())
	first, inserted, err := value.CreateAsyncVideoJob(ctx, job)
	if err != nil || !inserted {
		t.Fatalf("first create inserted=%v err=%v", inserted, err)
	}
	again, inserted, err := value.CreateAsyncVideoJob(ctx, job)
	if err != nil || inserted || again.ID != first.ID {
		t.Fatalf("idempotent create job=%#v inserted=%v err=%v", again, inserted, err)
	}
	changed := job
	changed.MediaURL = "https://cdn.example/other.mp4"
	if _, _, err := value.CreateAsyncVideoJob(ctx, changed); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("changed invocation error=%v, want conflict", err)
	}
}

func TestAsyncVideoJobRequeueClearsLease(t *testing.T) {
	ctx := context.Background()
	value, err := Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer value.Close()
	now := time.Now()
	job := durableVideoJob(now)
	_, _, _ = value.CreateAsyncVideoJob(ctx, job)
	leased, err := value.LeaseAsyncVideoJob(ctx, job.ID, now, time.Minute)
	if err != nil {
		t.Fatalf("LeaseAsyncVideoJob: %v", err)
	}
	if err := value.RequeueAsyncVideoJob(
		ctx, job.ID, leased.LeaseToken, "plugin reload", now.Add(time.Second),
	); err != nil {
		t.Fatalf("RequeueAsyncVideoJob: %v", err)
	}
	stored, err := value.GetAsyncVideoJob(ctx, job.ID)
	if err != nil || stored.State != domain.AsyncVideoJobPending ||
		stored.LeaseToken != "" || stored.Failure != "plugin reload" {
		t.Fatalf("requeued job=%#v err=%v", stored, err)
	}
}
