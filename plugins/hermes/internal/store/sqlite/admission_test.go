package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
)

func createAdmissionRun(
	t *testing.T,
	store *sqlite.Store,
	suffix string,
	sessionID string,
	messageID int64,
	trigger domain.TriggerKind,
) runningFixture {
	return createAdmissionRunForSpeaker(t, store, suffix, sessionID, messageID, trigger, "speaker-admission")
}

func createAdmissionRunForSpeaker(
	t *testing.T,
	store *sqlite.Store,
	suffix string,
	sessionID string,
	messageID int64,
	trigger domain.TriggerKind,
	speakerID string,
) runningFixture {
	t.Helper()
	event := inboxEvent(suffix, messageID)
	event.SessionID = sessionID
	event.Binding.SessionID = sessionID
	event.Binding.Principal.ID = speakerID
	event.Binding.Principal.Name = speakerID
	message := domain.InboundMessage{
		Text: "ambient", IsChatroom: true, SpeakerID: event.Binding.Principal.ID,
		OccurredAt: time.Now(),
	}
	priority := 10
	if trigger == domain.TriggerExplicit {
		message.Text = "@ccff hello"
		message.Mentioned = true
		priority = 100
	}
	if trigger == domain.TriggerControl {
		message.Text = "/hermes status"
		message.HermesCommand = "/status"
		priority = 1000
	}
	event.Payload, _ = json.Marshal(message)
	accepted, inserted, err := store.AcceptInbox(context.Background(), event)
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := store.MaterializeTurn(context.Background(), accepted.ID, priority)
	if err != nil {
		t.Fatalf("MaterializeTurn: %v", err)
	}
	route := domain.RouteChat
	lane := domain.LaneInteractive
	if trigger == domain.TriggerControl {
		route = domain.RouteControl
		lane = domain.LaneControl
	}
	_, run, err := store.RouteTurn(context.Background(), turn.ID, route, lane, time.Time{})
	if err != nil || run == nil {
		t.Fatalf("RouteTurn: run=%#v err=%v", run, err)
	}
	if run.TriggerKind != trigger {
		t.Fatalf("trigger=%s, want %s", run.TriggerKind, trigger)
	}
	return runningFixture{event: accepted, turn: turn, run: *run}
}

func TestInteractiveRunsFromDifferentGroupSpeakersCanLeaseConcurrently(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:admission-parallel"
	first := createAdmissionRunForSpeaker(t, store, "parallel-first", sessionID, 1, domain.TriggerExplicit, "speaker-a")
	second := createAdmissionRunForSpeaker(t, store, "parallel-second", sessionID, 2, domain.TriggerExplicit, "speaker-b")
	if first.run.ID == second.run.ID {
		t.Fatal("parallel fixtures reused a run id")
	}
	firstLeased, err := store.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil || firstLeased.ID != first.run.ID {
		t.Fatalf("lease first=%#v err=%v", firstLeased, err)
	}
	if err := store.MarkRunRunning(ctx, firstLeased.ID, firstLeased.LeaseToken); err != nil {
		t.Fatalf("mark first running: %v", err)
	}
	secondLeased, err := store.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil || secondLeased.ID != second.run.ID {
		t.Fatalf("different speaker was blocked by first run: second=%#v err=%v", secondLeased, err)
	}

	// Admission preemption is scoped by the same verified speaker as leasing.
	third := createAdmissionRunForSpeaker(t, store, "parallel-third", sessionID, 3, domain.TriggerAmbient, "speaker-c")
	result, err := store.ReconcileRunAdmission(ctx, third.run.ID, domain.RunAdmissionActive)
	if err != nil {
		t.Fatalf("admit independent ambient: %v", err)
	}
	if len(result.CancelledRunIDs) != 0 {
		t.Fatalf("independent speaker was preempted: %#v", result)
	}
}

func TestAdmissionCoalescesAmbientAndForegroundCancelsLatest(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:admission-coalesce"

	first := createAdmissionRun(t, store, "admission-first", sessionID, 1, domain.TriggerAmbient)
	if _, err := store.ReconcileRunAdmission(ctx, first.run.ID, domain.RunAdmissionActive); err != nil {
		t.Fatalf("admit first ambient: %v", err)
	}
	second := createAdmissionRun(t, store, "admission-second", sessionID, 2, domain.TriggerAmbient)
	secondAdmission, err := store.ReconcileRunAdmission(ctx, second.run.ID, domain.RunAdmissionActive)
	if err != nil {
		t.Fatalf("admit second ambient: %v", err)
	}
	if len(secondAdmission.CancelledRunIDs) != 1 || secondAdmission.CancelledRunIDs[0] != first.run.ID {
		t.Fatalf("second admission=%#v", secondAdmission)
	}

	foreground := createAdmissionRun(t, store, "admission-explicit", sessionID, 3, domain.TriggerExplicit)
	foregroundAdmission, err := store.ReconcileRunAdmission(ctx, foreground.run.ID, domain.RunAdmissionActive)
	if err != nil {
		t.Fatalf("admit foreground: %v", err)
	}
	if len(foregroundAdmission.CancelledRunIDs) != 1 || foregroundAdmission.CancelledRunIDs[0] != second.run.ID {
		t.Fatalf("foreground admission=%#v", foregroundAdmission)
	}
	for _, runID := range []string{first.run.ID, second.run.ID} {
		run, err := store.GetRun(ctx, runID)
		if err != nil || run.State != domain.RunCancelled {
			t.Fatalf("ambient run %s=%#v err=%v", runID, run, err)
		}
	}
	leased, err := store.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil || leased.ID != foreground.run.ID {
		t.Fatalf("leased=%#v err=%v, want foreground", leased, err)
	}
}

func TestAdmissionInterruptsRunningAmbientBeforeForeground(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:admission-running"

	ambient := createAdmissionRun(t, store, "admission-running-ambient", sessionID, 1, domain.TriggerAmbient)
	leasedAmbient, err := store.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil || leasedAmbient.ID != ambient.run.ID {
		t.Fatalf("lease ambient=%#v err=%v", leasedAmbient, err)
	}
	if err := store.MarkRunRunning(ctx, leasedAmbient.ID, leasedAmbient.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	foreground := createAdmissionRun(t, store, "admission-running-explicit", sessionID, 2, domain.TriggerExplicit)
	result, err := store.ReconcileRunAdmission(ctx, foreground.run.ID, domain.RunAdmissionActive)
	if err != nil {
		t.Fatalf("admit foreground: %v", err)
	}
	if len(result.InterruptRunIDs) != 1 || result.InterruptRunIDs[0] != ambient.run.ID {
		t.Fatalf("admission=%#v", result)
	}
	storedAmbient, err := store.GetRun(ctx, ambient.run.ID)
	if err != nil || storedAmbient.State != domain.RunCancelRequested {
		t.Fatalf("ambient=%#v err=%v", storedAmbient, err)
	}
	if run, err := store.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreground overtook active cancellation: run=%#v err=%v", run, err)
	}
	if err := store.MarkRunCancelled(ctx, ambient.run.ID, leasedAmbient.LeaseToken); err != nil {
		t.Fatalf("MarkRunCancelled: %v", err)
	}
	leasedForeground, err := store.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil || leasedForeground.ID != foreground.run.ID {
		t.Fatalf("lease foreground=%#v err=%v", leasedForeground, err)
	}
}

func TestOlderAmbientRoutedAfterForegroundIsSuperseded(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:admission-route-race"

	ambientEvent := inboxEvent("admission-race-ambient", 1)
	ambientEvent.SessionID = sessionID
	ambientEvent.Binding.SessionID = sessionID
	// The race being tested is within one verified participant's admission
	// lane.  Different verified speakers in a group intentionally have
	// independent lanes now.
	ambientEvent.Binding.Principal.ID = "same-verified-speaker"
	ambientEvent.Payload, _ = json.Marshal(domain.InboundMessage{
		Text: "older", IsChatroom: true, SpeakerID: "ambient", OccurredAt: time.Now(),
	})
	ambientAccepted, _, err := store.AcceptInbox(ctx, ambientEvent)
	if err != nil {
		t.Fatalf("accept ambient: %v", err)
	}
	explicitEvent := inboxEvent("admission-race-explicit", 2)
	explicitEvent.SessionID = sessionID
	explicitEvent.Binding.SessionID = sessionID
	explicitEvent.Binding.Principal.ID = "same-verified-speaker"
	explicitEvent.Payload, _ = json.Marshal(domain.InboundMessage{
		Text: "@ccff now", IsChatroom: true, Mentioned: true, SpeakerID: "user", OccurredAt: time.Now(),
	})
	explicitAccepted, _, err := store.AcceptInbox(ctx, explicitEvent)
	if err != nil {
		t.Fatalf("accept explicit: %v", err)
	}

	explicitTurn, _ := store.MaterializeTurn(ctx, explicitAccepted.ID, 100)
	_, explicitRun, err := store.RouteTurn(ctx, explicitTurn.ID, domain.RouteChat, domain.LaneInteractive, time.Time{})
	if err != nil {
		t.Fatalf("route explicit: %v", err)
	}
	if _, err := store.ReconcileRunAdmission(ctx, explicitRun.ID, domain.RunAdmissionActive); err != nil {
		t.Fatalf("admit explicit: %v", err)
	}
	ambientTurn, _ := store.MaterializeTurn(ctx, ambientAccepted.ID, 10)
	_, ambientRun, err := store.RouteTurn(ctx, ambientTurn.ID, domain.RouteChat, domain.LaneInteractive, time.Time{})
	if err != nil {
		t.Fatalf("route ambient: %v", err)
	}
	result, err := store.ReconcileRunAdmission(ctx, ambientRun.ID, domain.RunAdmissionActive)
	if err != nil || !result.CurrentSuperseded {
		t.Fatalf("ambient admission=%#v err=%v", result, err)
	}
	storedAmbient, _ := store.GetRun(ctx, ambientRun.ID)
	if storedAmbient.State != domain.RunCancelled {
		t.Fatalf("ambient state=%s", storedAmbient.State)
	}
}

func TestAdmissionModesAndReservedTriggerLease(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx := context.Background()
	ambient := createAdmissionRun(t, store, "admission-mode-ambient", "chatroom:mode-a", 1, domain.TriggerAmbient)
	explicit := createAdmissionRun(t, store, "admission-mode-explicit", "chatroom:mode-b", 2, domain.TriggerExplicit)

	if _, err := store.ReconcileRunAdmission(ctx, explicit.run.ID, domain.RunAdmissionOff); err != nil {
		t.Fatalf("off admission: %v", err)
	}
	leased, err := store.LeaseNextRunByTrigger(
		ctx, domain.LaneInteractive, domain.TriggerExplicit, time.Now(), time.Minute,
	)
	if err != nil || leased.ID != explicit.run.ID {
		t.Fatalf("reserved lease=%#v err=%v", leased, err)
	}
	storedAmbient, _ := store.GetRun(ctx, ambient.run.ID)
	if storedAmbient.State != domain.RunQueued {
		t.Fatalf("off mode changed ambient state=%s", storedAmbient.State)
	}
}

func TestQueuedAdmissionCancelsPendingButDoesNotInterruptRunningAmbient(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx := context.Background()

	pendingSession := "chatroom:admission-queued-pending"
	pending := createAdmissionRun(t, store, "admission-queued-pending", pendingSession, 1, domain.TriggerAmbient)
	foreground := createAdmissionRun(t, store, "admission-queued-foreground", pendingSession, 2, domain.TriggerExplicit)
	result, err := store.ReconcileRunAdmission(ctx, foreground.run.ID, domain.RunAdmissionQueued)
	if err != nil {
		t.Fatalf("queued admission with pending ambient: %v", err)
	}
	if len(result.CancelledRunIDs) != 1 || result.CancelledRunIDs[0] != pending.run.ID || len(result.InterruptRunIDs) != 0 {
		t.Fatalf("pending result=%#v", result)
	}

	runningSession := "chatroom:admission-queued-running"
	running := createAdmissionRun(t, store, "admission-queued-running", runningSession, 3, domain.TriggerAmbient)
	leased, err := store.LeaseNextRunByTrigger(
		ctx, domain.LaneInteractive, domain.TriggerAmbient, time.Now(), time.Minute,
	)
	if err != nil || leased.ID != running.run.ID {
		t.Fatalf("lease running ambient=%#v err=%v", leased, err)
	}
	if err := store.MarkRunRunning(ctx, leased.ID, leased.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	runningForeground := createAdmissionRun(t, store, "admission-queued-running-foreground", runningSession, 4, domain.TriggerExplicit)
	result, err = store.ReconcileRunAdmission(ctx, runningForeground.run.ID, domain.RunAdmissionQueued)
	if err != nil {
		t.Fatalf("queued admission with running ambient: %v", err)
	}
	if len(result.CancelledRunIDs) != 0 || len(result.InterruptRunIDs) != 0 {
		t.Fatalf("running result=%#v", result)
	}
	storedRunning, err := store.GetRun(ctx, running.run.ID)
	if err != nil || storedRunning.State != domain.RunRunning {
		t.Fatalf("running ambient=%#v err=%v", storedRunning, err)
	}
}

func TestAdmissionRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	run := createAdmissionRun(t, store, "admission-invalid-mode", "chatroom:admission-invalid", 1, domain.TriggerAmbient)
	if _, err := store.ReconcileRunAdmission(
		context.Background(), run.run.ID, domain.RunAdmissionMode("invalid"),
	); !errors.Is(err, storeport.ErrInvalid) {
		t.Fatalf("ReconcileRunAdmission error=%v, want ErrInvalid", err)
	}
}
