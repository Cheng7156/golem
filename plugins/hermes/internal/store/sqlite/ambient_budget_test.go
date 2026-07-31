package sqlite_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/store/sqlite"
)

func startAmbientBudgetRun(
	t *testing.T,
	store *sqlite.Store,
	suffix string,
	sessionID string,
	messageID int64,
) runningFixture {
	t.Helper()
	fixture := createAdmissionRun(t, store, suffix, sessionID, messageID, domain.TriggerAmbient)
	leased, err := store.LeaseNextRun(context.Background(), domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("LeaseNextRun: %v", err)
	}
	if leased.ID != fixture.run.ID {
		t.Fatalf("leased=%s want=%s", leased.ID, fixture.run.ID)
	}
	if err := store.MarkRunRunning(context.Background(), leased.ID, leased.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	fixture.run = leased
	fixture.run.State = domain.RunRunning
	return fixture
}

func TestAmbientReplyBudgetConsumesOnlyVisibleCommit(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:durable-ambient-budget"
	now := time.Now()

	first := startAmbientBudgetRun(t, store, "budget-first", sessionID, 1)
	allowed, err := store.ReserveAmbientReply(ctx, first.run.ID, now, 20*time.Second, time.Minute, 2)
	if err != nil || !allowed {
		t.Fatalf("reserve first: allowed=%v err=%v", allowed, err)
	}
	payload, _ := json.Marshal(domain.TextOutput{Content: "visible"})
	if _, err := store.CommitRunSuccess(ctx, first.run.ID, first.run.LeaseToken, []domain.OutboxDraft{{
		SessionID: sessionID, ReceiverID: "room", Kind: "text", Payload: payload,
	}}); err != nil {
		t.Fatalf("commit visible: %v", err)
	}

	second := startAmbientBudgetRun(t, store, "budget-second", sessionID, 2)
	allowed, err = store.ReserveAmbientReply(ctx, second.run.ID, now.Add(5*time.Second), 20*time.Second, time.Minute, 2)
	if err != nil || allowed {
		t.Fatalf("cooldown reserve: allowed=%v err=%v", allowed, err)
	}
	if err := store.RequestRunCancel(ctx, second.run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunCancelled(ctx, second.run.ID, second.run.LeaseToken); err != nil {
		t.Fatal(err)
	}

	third := startAmbientBudgetRun(t, store, "budget-silent", sessionID, 3)
	allowed, err = store.ReserveAmbientReply(ctx, third.run.ID, now.Add(21*time.Second), 20*time.Second, time.Minute, 2)
	if err != nil || !allowed {
		t.Fatalf("reserve silent: allowed=%v err=%v", allowed, err)
	}
	if _, err := store.CommitRunSuccess(ctx, third.run.ID, third.run.LeaseToken, nil); err != nil {
		t.Fatalf("commit silent: %v", err)
	}

	fourth := startAmbientBudgetRun(t, store, "budget-after-silent", sessionID, 4)
	allowed, err = store.ReserveAmbientReply(ctx, fourth.run.ID, now.Add(22*time.Second), 20*time.Second, time.Minute, 2)
	if err != nil || !allowed {
		t.Fatalf("silent result consumed budget: allowed=%v err=%v", allowed, err)
	}
}

func TestAmbientReplyBudgetAllowsZeroCooldownButKeepsWindowQuota(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:zero-ambient-cooldown"
	now := time.Now()
	payload, _ := json.Marshal(domain.TextOutput{Content: "visible"})

	first := startAmbientBudgetRun(t, store, "zero-cooldown-first", sessionID, 1)
	allowed, err := store.ReserveAmbientReply(ctx, first.run.ID, now, 0, time.Minute, 2)
	if err != nil || !allowed {
		t.Fatalf("reserve first: allowed=%v err=%v", allowed, err)
	}
	if _, err := store.CommitRunSuccess(ctx, first.run.ID, first.run.LeaseToken, []domain.OutboxDraft{{
		SessionID: sessionID, ReceiverID: "room", Kind: "text", Payload: payload,
	}}); err != nil {
		t.Fatalf("commit first: %v", err)
	}

	second := startAmbientBudgetRun(t, store, "zero-cooldown-second", sessionID, 2)
	allowed, err = store.ReserveAmbientReply(ctx, second.run.ID, now, 0, time.Minute, 2)
	if err != nil || !allowed {
		t.Fatalf("reserve second: allowed=%v err=%v", allowed, err)
	}
	if _, err := store.CommitRunSuccess(ctx, second.run.ID, second.run.LeaseToken, []domain.OutboxDraft{{
		SessionID: sessionID, ReceiverID: "room", Kind: "text", Payload: payload,
	}}); err != nil {
		t.Fatalf("commit second: %v", err)
	}

	third := startAmbientBudgetRun(t, store, "zero-cooldown-quota", sessionID, 3)
	allowed, err = store.ReserveAmbientReply(ctx, third.run.ID, now, 0, time.Minute, 2)
	if err != nil || allowed {
		t.Fatalf("window quota reserve: allowed=%v err=%v", allowed, err)
	}
}

func TestAmbientReplyBudgetAllowsUnlimitedWindowReplies(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:unlimited-ambient-budget"
	now := time.Now()
	payload, _ := json.Marshal(domain.TextOutput{Content: "visible"})

	for index := 0; index < 3; index++ {
		fixture := startAmbientBudgetRun(
			t, store, "unlimited-budget-"+string(rune('a'+index)), sessionID, int64(index+1),
		)
		allowed, err := store.ReserveAmbientReply(ctx, fixture.run.ID, now, 0, time.Minute, 0)
		if err != nil || !allowed {
			t.Fatalf("reserve reply %d: allowed=%v err=%v", index+1, allowed, err)
		}
		if _, err := store.CommitRunSuccess(ctx, fixture.run.ID, fixture.run.LeaseToken, []domain.OutboxDraft{{
			SessionID: sessionID, ReceiverID: "room", Kind: "text", Payload: payload,
		}}); err != nil {
			t.Fatalf("commit reply %d: %v", index+1, err)
		}
	}
}
