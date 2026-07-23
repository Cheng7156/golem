package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func cronRegistration(fixture runningFixture) domain.CronDeliveryRegistration {
	return domain.CronDeliveryRegistration{
		Profile: "default", JobID: "job_gateway_status",
		ChatID: fixture.event.SessionID + "|interactive", ParentRunID: fixture.run.ID,
	}
}

func TestCronDeliveryRegisterBindsActiveParentRun(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "cron-register")
	registration := cronRegistration(fixture)

	first, err := value.RegisterCronDelivery(context.Background(), registration)
	if err != nil {
		t.Fatalf("RegisterCronDelivery: %v", err)
	}
	again, err := value.RegisterCronDelivery(context.Background(), registration)
	if err != nil {
		t.Fatalf("idempotent register: %v", err)
	}
	if first.ID != again.ID || first.Binding != fixture.event.Binding {
		t.Fatalf("first=%#v again=%#v binding=%#v", first, again, fixture.event.Binding)
	}
}

func TestCronDeliveryCommitIsDurableAndIdempotent(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "cron-commit")
	binding, err := value.RegisterCronDelivery(
		context.Background(), cronRegistration(fixture),
	)
	if err != nil {
		t.Fatalf("RegisterCronDelivery: %v", err)
	}
	commit := domain.CronDeliveryCommit{
		Profile: binding.Profile, JobID: binding.JobID,
		ChatID: binding.ChatID, DeliveryID: "job_gateway_status:2026-07-16T12:00:00+08:00",
		Content: "网关状态正常",
	}
	first, err := value.CommitCronDelivery(context.Background(), commit)
	if err != nil {
		t.Fatalf("CommitCronDelivery: %v", err)
	}
	again, err := value.CommitCronDelivery(context.Background(), commit)
	if err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if first.OutboxID == "" || first != again {
		t.Fatalf("first=%#v again=%#v", first, again)
	}
	if first.DeliveryState != "queued" {
		t.Fatalf("initial delivery state=%q", first.DeliveryState)
	}
	outbox, err := value.GetOutbox(context.Background(), first.OutboxID)
	if err != nil || outbox.State != domain.OutboxPending || outbox.ReceiverID != fixture.event.Binding.ReceiverID {
		t.Fatalf("outbox=%#v err=%v", outbox, err)
	}
	var payload domain.TextOutput
	if err := json.Unmarshal(outbox.Payload, &payload); err != nil || payload.Content != commit.Content {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
	leased, err := value.LeaseNextOutbox(context.Background(), time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.MarkOutboxSending(context.Background(), leased.ID, leased.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := value.MarkOutboxSent(context.Background(), leased.ID, leased.LeaseToken, 7001, time.Now()); err != nil {
		t.Fatal(err)
	}
	delivered, err := value.CommitCronDelivery(context.Background(), commit)
	if err != nil || delivered.DeliveryState != "delivered" {
		t.Fatalf("delivered=%#v err=%v", delivered, err)
	}
}

func TestCronDeliveryRejectsBindingAndContentReuse(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "cron-conflict")
	binding, _ := value.RegisterCronDelivery(
		context.Background(), cronRegistration(fixture),
	)
	commit := domain.CronDeliveryCommit{
		Profile: binding.Profile, JobID: binding.JobID,
		ChatID: binding.ChatID, DeliveryID: "fire-1", Content: "first",
	}
	if _, err := value.CommitCronDelivery(context.Background(), commit); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	commit.Content = "changed"
	if _, err := value.CommitCronDelivery(context.Background(), commit); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("content reuse err=%v, want conflict", err)
	}
	commit.Content = "first"
	commit.ChatID += "-wrong"
	if _, err := value.CommitCronDelivery(context.Background(), commit); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("binding mismatch err=%v, want conflict", err)
	}
}
