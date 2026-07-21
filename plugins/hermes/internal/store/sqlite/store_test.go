package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	value, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := value.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return value
}

func inboxEvent(suffix string, messageID int64) domain.InboxEvent {
	sessionID := "chatroom:room-" + suffix
	return domain.InboxEvent{
		ID:         "event-" + suffix,
		DedupeKey:  "wechat/message/" + suffix,
		MessageID:  messageID,
		Topic:      "message.text",
		SessionID:  sessionID,
		OccurredAt: time.Now(),
		Binding: domain.ChannelBinding{
			Channel:    "wechat",
			SessionID:  sessionID,
			ReceiverID: "room-" + suffix,
			Principal: domain.Principal{
				ID:   "wxid-" + suffix,
				Name: "tester",
			},
		},
		Payload: json.RawMessage(`{"text":"hello"}`),
	}
}

type runningFixture struct {
	event domain.InboxEvent
	turn  domain.Turn
	run   domain.Run
}

func createRunningRun(t *testing.T, value *sqlite.Store, suffix string) runningFixture {
	t.Helper()
	ctx := context.Background()
	event, inserted, err := value.AcceptInbox(ctx, inboxEvent(suffix, 100))
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := value.CreateTurn(ctx, domain.Turn{
		ID:        "turn-" + suffix,
		EventID:   event.ID,
		SessionID: event.SessionID,
		Priority:  100,
	})
	if err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if err := value.TransitionTurn(ctx, turn.ID, domain.TurnOrdered, domain.RouteUnset); err != nil {
		t.Fatalf("Turn ordered: %v", err)
	}
	if err := value.TransitionTurn(ctx, turn.ID, domain.TurnRouted, domain.RouteChat); err != nil {
		t.Fatalf("Turn routed: %v", err)
	}
	if err := value.TransitionTurn(ctx, turn.ID, domain.TurnQueuedInteractive, domain.RouteUnset); err != nil {
		t.Fatalf("Turn queued: %v", err)
	}
	run, err := value.CreateRun(ctx, domain.Run{
		ID:        "run-" + suffix,
		TurnID:    turn.ID,
		SessionID: event.SessionID,
		Lane:      domain.LaneInteractive,
		Deadline:  time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	leased, err := value.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("LeaseNextRun: %v", err)
	}
	if leased.ID != run.ID {
		t.Fatalf("leased run=%s, want %s", leased.ID, run.ID)
	}
	if err := value.MarkRunRunning(ctx, leased.ID, leased.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	leased.State = domain.RunRunning
	return runningFixture{event: event, turn: turn, run: leased}
}

func createQueuedRunForSession(
	t *testing.T,
	value *sqlite.Store,
	suffix string,
	sessionID string,
	messageID int64,
) runningFixture {
	t.Helper()
	ctx := context.Background()
	eventValue := inboxEvent(suffix, messageID)
	eventValue.SessionID = sessionID
	eventValue.Binding.SessionID = sessionID
	event, inserted, err := value.AcceptInbox(ctx, eventValue)
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := value.MaterializeTurn(ctx, event.ID, 100)
	if err != nil {
		t.Fatalf("MaterializeTurn: %v", err)
	}
	_, run, err := value.RouteTurn(ctx, turn.ID, domain.RouteChat, domain.LaneInteractive, time.Time{})
	if err != nil || run == nil {
		t.Fatalf("RouteTurn: run=%#v err=%v", run, err)
	}
	return runningFixture{event: event, turn: turn, run: *run}
}

func TestAcceptInboxIsDurableAndIdempotent(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	ctx := context.Background()
	first := inboxEvent("dedupe", 41)
	stored, inserted, err := value.AcceptInbox(ctx, first)
	if err != nil || !inserted {
		t.Fatalf("first accept: inserted=%v err=%v", inserted, err)
	}
	duplicate := first
	duplicate.ID = "different-event-id"
	again, inserted, err := value.AcceptInbox(ctx, duplicate)
	if err != nil {
		t.Fatalf("duplicate accept: %v", err)
	}
	if inserted {
		t.Fatal("duplicate event was inserted")
	}
	if again.ID != stored.ID || again.AcceptSeq != stored.AcceptSeq {
		t.Fatalf("duplicate did not resolve original: %#v != %#v", again, stored)
	}
	items, err := value.ListInbox(ctx, domain.InboxAccepted, 10)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("inbox size=%d, want 1", len(items))
	}
}

func TestConcurrentInboxAcceptanceInsertsExactlyOnce(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	event := inboxEvent("concurrent", 42)
	var inserted atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := value.AcceptInbox(context.Background(), event)
			if created {
				inserted.Add(1)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("AcceptInbox: %v", err)
	}
	if got := inserted.Load(); got != 1 {
		t.Fatalf("inserted=%d, want 1", got)
	}
}

func TestMaterializeAndRouteTurnUseRecoverableStageBoundaries(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	ctx := context.Background()
	event, inserted, err := value.AcceptInbox(ctx, inboxEvent("materialize", 43))
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := value.MaterializeTurn(ctx, event.ID, 75)
	if err != nil {
		t.Fatalf("MaterializeTurn: %v", err)
	}
	if turn.State != domain.TurnOrdered || turn.Priority != 75 {
		t.Fatalf("unexpected materialized turn: %#v", turn)
	}
	again, err := value.MaterializeTurn(ctx, event.ID, 1)
	if err != nil {
		t.Fatalf("idempotent MaterializeTurn: %v", err)
	}
	if again.ID != turn.ID || again.Priority != 75 {
		t.Fatalf("materialize did not return original turn: %#v", again)
	}
	storedEvent, err := value.GetInbox(ctx, event.ID)
	if err != nil || storedEvent.Status != domain.InboxOrdered {
		t.Fatalf("inbox was not atomically ordered: %#v err=%v", storedEvent, err)
	}
	routed, run, err := value.RouteTurn(
		ctx,
		turn.ID,
		domain.RouteChat,
		domain.LaneInteractive,
		time.Now().Add(30*time.Second),
	)
	if err != nil {
		t.Fatalf("RouteTurn: %v", err)
	}
	if routed.State != domain.TurnQueuedInteractive || run == nil || run.State != domain.RunQueued {
		t.Fatalf("route and run were not committed together: turn=%#v run=%#v", routed, run)
	}
	storedEvent, err = value.GetInbox(ctx, event.ID)
	if err != nil || storedEvent.Status != domain.InboxRouted {
		t.Fatalf("inbox was not atomically routed: %#v err=%v", storedEvent, err)
	}
	if _, _, err := value.RouteTurn(
		ctx,
		turn.ID,
		domain.RouteChat,
		domain.LaneInteractive,
		time.Now().Add(30*time.Second),
	); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("second route error=%v, want conflict", err)
	}
}

func TestInboxSurvivesStoreReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hermes.db")
	ctx := context.Background()
	first, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	event, inserted, err := first.AcceptInbox(ctx, inboxEvent("reopen", 44))
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	second, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	defer second.Close()
	stored, err := second.GetInbox(ctx, event.ID)
	if err != nil {
		t.Fatalf("GetInbox after reopen: %v", err)
	}
	if stored.DedupeKey != event.DedupeKey || stored.AcceptSeq == 0 {
		t.Fatalf("unexpected restored inbox event: %#v", stored)
	}
}

func TestRunCommitAndOutboxDeliveryAreTransactionalAndOrdered(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "commit")
	ctx := context.Background()
	drafts := []domain.OutboxDraft{
		{
			SessionID:  fixture.event.SessionID,
			ReceiverID: fixture.event.Binding.ReceiverID,
			Kind:       "text",
			Payload:    json.RawMessage(`{"content":"first"}`),
		},
		{
			SessionID:  fixture.event.SessionID,
			ReceiverID: fixture.event.Binding.ReceiverID,
			Kind:       "text",
			Payload:    json.RawMessage(`{"content":"second"}`),
		},
	}
	items, err := value.CommitRunSuccess(ctx, fixture.run.ID, fixture.run.LeaseToken, drafts)
	if err != nil {
		t.Fatalf("CommitRunSuccess: %v", err)
	}
	if len(items) != 2 || items[0].Sequence != 1 || items[1].Sequence != 2 {
		t.Fatalf("unexpected outbox items: %#v", items)
	}
	run, err := value.GetRun(ctx, fixture.run.ID)
	if err != nil || run.State != domain.RunSucceeded {
		t.Fatalf("run state=%s err=%v", run.State, err)
	}
	turn, err := value.GetTurn(ctx, fixture.turn.ID)
	if err != nil || turn.State != domain.TurnCompleted {
		t.Fatalf("turn state=%s err=%v", turn.State, err)
	}

	now := time.Now().Add(time.Second)
	first, err := value.LeaseNextOutbox(ctx, now, time.Minute)
	if err != nil {
		t.Fatalf("Lease first: %v", err)
	}
	if first.ID != items[0].ID {
		t.Fatalf("leased %s, want first %s", first.ID, items[0].ID)
	}
	if err := value.MarkOutboxSent(ctx, first.ID, first.LeaseToken, 0, time.Now()); !errors.Is(err, storeport.ErrInvalid) {
		t.Fatalf("zero receipt error=%v, want ErrInvalid", err)
	}
	retryAt := now.Add(time.Minute)
	if err := value.MarkOutboxRetry(ctx, first.ID, first.LeaseToken, "ambiguous", "response lost", retryAt); err != nil {
		t.Fatalf("MarkOutboxRetry: %v", err)
	}
	first, err = value.LeaseNextOutbox(ctx, retryAt, time.Minute)
	if err != nil {
		t.Fatalf("re-lease first: %v", err)
	}
	second, err := value.LeaseNextOutbox(ctx, retryAt, time.Minute)
	if err != nil || second.ID != items[1].ID {
		t.Fatalf("later item blocked by active retry: item=%#v err=%v", second, err)
	}
	if err := value.MarkOutboxSent(ctx, second.ID, second.LeaseToken, 9002, time.Now()); err != nil {
		t.Fatalf("Mark second sent: %v", err)
	}
	if err := value.MarkOutboxSent(ctx, first.ID, first.LeaseToken, 9001, time.Now()); err != nil {
		t.Fatalf("Mark first sent: %v", err)
	}
	attempts, err := value.ListDeliveryAttempts(ctx, first.ID)
	if err != nil {
		t.Fatalf("ListDeliveryAttempts: %v", err)
	}
	if len(attempts) != 2 || attempts[0].Outcome != "ambiguous" || attempts[1].Outcome != "sent" {
		t.Fatalf("unexpected attempts: %#v", attempts)
	}
}

func TestRelayRunResultCommitIsIdempotentAfterRunCompleted(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "relay-result-idempotent")
	ctx := context.Background()
	proposal := domain.RelayRunResult{ProposalID: "proposal-1", InvocationID: "invoke-1",
		RunID: fixture.run.ID, ResultKind: "visible_reply", ResultHash: "hash-1"}
	drafts := []domain.OutboxDraft{{SessionID: fixture.event.SessionID,
		ReceiverID: fixture.event.Binding.ReceiverID, Kind: "text", Payload: json.RawMessage(`{"content":"hello"}`)}}
	items, err := value.CommitRelayRunResult(ctx, fixture.run.ID, fixture.run.LeaseToken, proposal, drafts)
	if err != nil || len(items) != 1 {
		t.Fatalf("first CommitRelayRunResult items=%d err=%v", len(items), err)
	}
	if _, err := value.CommitRelayRunResult(ctx, fixture.run.ID, "lost-lease", proposal, drafts); err != nil {
		t.Fatalf("duplicate CommitRelayRunResult after completion: %v", err)
	}
	conflict := proposal
	conflict.ResultHash = "different"
	if _, err := value.CommitRelayRunResult(ctx, fixture.run.ID, "lost-lease", conflict, drafts); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("conflicting duplicate error=%v, want conflict", err)
	}
	stored, err := value.GetRelayRunResult(ctx, proposal.ProposalID)
	if err != nil || len(stored.OutboxIDs) != 1 || stored.OutboxIDs[0] != items[0].ID {
		t.Fatalf("stored relay result=%#v err=%v", stored, err)
	}
}

func TestWrongRunLeaseCannotCommitOutbox(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "wrong-lease")
	_, err := value.CommitRunSuccess(context.Background(), fixture.run.ID, "wrong", []domain.OutboxDraft{{
		SessionID:  fixture.event.SessionID,
		ReceiverID: fixture.event.Binding.ReceiverID,
		Kind:       "text",
		Payload:    json.RawMessage(`{"content":"must not send"}`),
	}})
	if !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("CommitRunSuccess error=%v, want conflict", err)
	}
	run, getErr := value.GetRun(context.Background(), fixture.run.ID)
	if getErr != nil || run.State != domain.RunRunning {
		t.Fatalf("run state changed after rejected commit: state=%s err=%v", run.State, getErr)
	}
}

func TestRecoveryRequeuesRunningRunsAndLeasedOutbox(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	ctx := context.Background()
	running := createRunningRun(t, value, "recover-run")
	completed := createRunningRun(t, value, "recover-outbox")
	items, err := value.CommitRunSuccess(ctx, completed.run.ID, completed.run.LeaseToken, []domain.OutboxDraft{{
		SessionID:  completed.event.SessionID,
		ReceiverID: completed.event.Binding.ReceiverID,
		Kind:       "text",
		Payload:    json.RawMessage(`{"content":"retry me"}`),
	}})
	if err != nil {
		t.Fatalf("CommitRunSuccess: %v", err)
	}
	leasedOutbox, err := value.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || leasedOutbox.ID != items[0].ID {
		t.Fatalf("LeaseNextOutbox: item=%s err=%v", leasedOutbox.ID, err)
	}

	recoveredAt := time.Now().Add(2 * time.Second)
	result, err := value.Recover(ctx, recoveredAt)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.RunsRecovered != 1 || result.OutboxRecovered != 1 {
		t.Fatalf("unexpected recovery result: %#v", result)
	}
	run, err := value.GetRun(ctx, running.run.ID)
	if err != nil || run.State != domain.RunRetryWait || run.LeaseToken != "" {
		t.Fatalf("run not recovered: %#v err=%v", run, err)
	}
	outbox, err := value.GetOutbox(ctx, leasedOutbox.ID)
	if err != nil || outbox.State != domain.OutboxRetryWait || outbox.LeaseToken != "" {
		t.Fatalf("outbox not recovered: %#v err=%v", outbox, err)
	}
	if _, err := value.LeaseNextRun(ctx, domain.LaneInteractive, recoveredAt, time.Minute); err != nil {
		t.Fatalf("recovered run is not leasable: %v", err)
	}
}

func TestRetryWaitRunBlocksLaterRunInSameSession(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	ctx := context.Background()
	sessionID := "chatroom:strict-fifo"
	first := createQueuedRunForSession(t, value, "fifo-first", sessionID, 1)
	leasedFirst, err := value.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute)
	if err != nil || leasedFirst.ID != first.run.ID {
		t.Fatalf("Lease first: run=%#v err=%v", leasedFirst, err)
	}
	if err := value.MarkRunRunning(ctx, leasedFirst.ID, leasedFirst.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	retryAt := time.Now().Add(time.Minute)
	if err := value.FailRun(ctx, leasedFirst.ID, leasedFirst.LeaseToken, "temporary", true, retryAt); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	second := createQueuedRunForSession(t, value, "fifo-second", sessionID, 2)

	if run, err := value.LeaseNextRun(ctx, domain.LaneInteractive, time.Now(), time.Minute); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("later Run overtook retry_wait head: run=%#v err=%v", run, err)
	}
	retriedFirst, err := value.LeaseNextRun(ctx, domain.LaneInteractive, retryAt.Add(time.Second), time.Minute)
	if err != nil || retriedFirst.ID != first.run.ID {
		t.Fatalf("Lease retried first: run=%#v err=%v", retriedFirst, err)
	}
	if err := value.MarkRunRunning(ctx, retriedFirst.ID, retriedFirst.LeaseToken); err != nil {
		t.Fatalf("Mark retried first running: %v", err)
	}
	if _, err := value.CommitRunSuccess(ctx, retriedFirst.ID, retriedFirst.LeaseToken, nil); err != nil {
		t.Fatalf("Commit first: %v", err)
	}
	leasedSecond, err := value.LeaseNextRun(ctx, domain.LaneInteractive, retryAt.Add(2*time.Second), time.Minute)
	if err != nil || leasedSecond.ID != second.run.ID {
		t.Fatalf("Lease second after first completed: run=%#v err=%v", leasedSecond, err)
	}
}

func TestCooperativeCancellationKeepsLeaseUntilWorkerStops(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "cancel")
	ctx := context.Background()
	if err := value.RequestRunCancel(ctx, fixture.run.ID); err != nil {
		t.Fatalf("RequestRunCancel: %v", err)
	}
	run, err := value.GetRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != domain.RunCancelRequested || run.LeaseToken != fixture.run.LeaseToken {
		t.Fatalf("cancel request lost active lease: %#v", run)
	}
	if err := value.MarkRunCancelled(ctx, run.ID, run.LeaseToken); err != nil {
		t.Fatalf("MarkRunCancelled: %v", err)
	}
	run, err = value.GetRun(ctx, fixture.run.ID)
	if err != nil || run.State != domain.RunCancelled || run.LeaseToken != "" {
		t.Fatalf("run not cancelled: %#v err=%v", run, err)
	}
}

func TestRecoveryClosesRequestedCancellation(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "recover-cancel")
	ctx := context.Background()
	if err := value.RequestRunCancel(ctx, fixture.run.ID); err != nil {
		t.Fatalf("RequestRunCancel: %v", err)
	}
	result, err := value.Recover(ctx, time.Now())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.RunsRecovered != 1 {
		t.Fatalf("RunsRecovered=%d, want 1", result.RunsRecovered)
	}
	run, err := value.GetRun(ctx, fixture.run.ID)
	if err != nil || run.State != domain.RunCancelled {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	turn, err := value.GetTurn(ctx, fixture.turn.ID)
	if err != nil || turn.State != domain.TurnCancelled {
		t.Fatalf("turn=%#v err=%v", turn, err)
	}
	inbox, err := value.GetInbox(ctx, fixture.event.ID)
	if err != nil || inbox.Status != domain.InboxDone {
		t.Fatalf("inbox=%#v err=%v", inbox, err)
	}
}

func TestPermanentRunFailureClosesTurnAndInbox(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "permanent-failure")
	ctx := context.Background()
	if err := value.FailRun(ctx, fixture.run.ID, fixture.run.LeaseToken, "failed", false, time.Time{}); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	run, err := value.GetRun(ctx, fixture.run.ID)
	if err != nil || run.State != domain.RunFailed || run.LeaseToken != "" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	turn, err := value.GetTurn(ctx, fixture.turn.ID)
	if err != nil || turn.State != domain.TurnFailed {
		t.Fatalf("turn=%#v err=%v", turn, err)
	}
	inbox, err := value.GetInbox(ctx, fixture.event.ID)
	if err != nil || inbox.Status != domain.InboxFailed {
		t.Fatalf("inbox=%#v err=%v", inbox, err)
	}
}

func TestRunFailureAndUserNoticeCommitAtomically(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "failure-outbox")
	ctx := context.Background()
	items, err := value.CommitRunFailure(ctx, fixture.run.ID, fixture.run.LeaseToken, "gateway failed", []domain.OutboxDraft{{
		SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID,
		Kind: "text", Payload: json.RawMessage(`{"content":"try later"}`),
	}})
	if err != nil {
		t.Fatalf("CommitRunFailure: %v", err)
	}
	if len(items) != 1 || items[0].RunID != fixture.run.ID || items[0].State != domain.OutboxPending {
		t.Fatalf("outbox=%#v", items)
	}
	run, err := value.GetRun(ctx, fixture.run.ID)
	if err != nil || run.State != domain.RunFailed || run.LastError != "gateway failed" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	turn, err := value.GetTurn(ctx, fixture.turn.ID)
	if err != nil || turn.State != domain.TurnFailed {
		t.Fatalf("turn=%#v err=%v", turn, err)
	}
	inbox, err := value.GetInbox(ctx, fixture.event.ID)
	if err != nil || inbox.Status != domain.InboxFailed {
		t.Fatalf("inbox=%#v err=%v", inbox, err)
	}
}

func TestRunCheckpointIsLeaseFencedAndDurable(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "checkpoint")
	ctx := context.Background()
	checkpoint := json.RawMessage(`{"cursor":3}`)
	if err := value.SaveRunCheckpoint(ctx, fixture.run.ID, fixture.run.LeaseToken, checkpoint); err != nil {
		t.Fatalf("SaveRunCheckpoint: %v", err)
	}
	stored, err := value.GetRun(ctx, fixture.run.ID)
	if err != nil || string(stored.Checkpoint) != string(checkpoint) {
		t.Fatalf("checkpoint=%s err=%v", stored.Checkpoint, err)
	}
	if err := value.SaveRunCheckpoint(ctx, fixture.run.ID, "wrong-lease", json.RawMessage(`{"cursor":4}`)); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("wrong lease error=%v, want conflict", err)
	}
	stored, err = value.GetRun(ctx, fixture.run.ID)
	if err != nil || string(stored.Checkpoint) != string(checkpoint) {
		t.Fatalf("wrong lease changed checkpoint=%s err=%v", stored.Checkpoint, err)
	}
}

func TestDeadLetterUnblocksLaterOutboxItem(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "dead-letter")
	ctx := context.Background()
	items, err := value.CommitRunSuccess(ctx, fixture.run.ID, fixture.run.LeaseToken, []domain.OutboxDraft{
		{SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID, Kind: "text", Payload: json.RawMessage(`{"content":"first"}`)},
		{SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID, Kind: "text", Payload: json.RawMessage(`{"content":"second"}`)},
	})
	if err != nil {
		t.Fatalf("CommitRunSuccess: %v", err)
	}
	first, err := value.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || first.ID != items[0].ID {
		t.Fatalf("Lease first: item=%s err=%v", first.ID, err)
	}
	if err := value.MarkOutboxDeadLetter(ctx, first.ID, first.LeaseToken, "failed", "permanent"); err != nil {
		t.Fatalf("MarkOutboxDeadLetter: %v", err)
	}
	stored, err := value.GetOutbox(ctx, first.ID)
	if err != nil || stored.State != domain.OutboxDeadLetter {
		t.Fatalf("dead-letter item=%#v err=%v", stored, err)
	}
	second, err := value.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || second.ID != items[1].ID {
		t.Fatalf("Lease second: item=%s err=%v", second.ID, err)
	}
}

func TestExpiredOutboxLeaseCanBeReclaimed(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "outbox-reclaim")
	ctx := context.Background()
	items, err := value.CommitRunSuccess(ctx, fixture.run.ID, fixture.run.LeaseToken, []domain.OutboxDraft{{
		SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID,
		Kind: "text", Payload: json.RawMessage(`{"content":"retry"}`),
	}})
	if err != nil {
		t.Fatalf("CommitRunSuccess: %v", err)
	}
	leased, err := value.LeaseNextOutbox(ctx, time.Now(), time.Millisecond)
	if err != nil || leased.ID != items[0].ID {
		t.Fatalf("first lease=%#v err=%v", leased, err)
	}
	reclaimed, err := value.LeaseNextOutbox(ctx, leased.LeaseUntil.Add(time.Millisecond), time.Minute)
	if err != nil {
		t.Fatalf("reclaim expired lease: %v", err)
	}
	if reclaimed.ID != leased.ID || reclaimed.LeaseToken == leased.LeaseToken || reclaimed.Attempt != leased.Attempt+1 {
		t.Fatalf("reclaimed=%#v first=%#v", reclaimed, leased)
	}
}

func TestSessionCancellationExcludesControlRunAndCancelsActiveRun(t *testing.T) {
	t.Parallel()
	value := openStore(t)
	active := createRunningRun(t, value, "session-cancel")
	ctx := context.Background()
	controlEvent := inboxEvent("session-cancel-control", 200)
	controlEvent.SessionID = active.event.SessionID
	controlEvent.Binding.SessionID = active.event.SessionID
	controlEvent.Binding.ReceiverID = active.event.Binding.ReceiverID
	accepted, inserted, err := value.AcceptInbox(ctx, controlEvent)
	if err != nil || !inserted {
		t.Fatalf("Accept control inbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := value.MaterializeTurn(ctx, accepted.ID, 1000)
	if err != nil {
		t.Fatalf("Materialize control turn: %v", err)
	}
	_, controlRun, err := value.RouteTurn(ctx, turn.ID, domain.RouteControl, domain.LaneControl, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Route control turn: %v", err)
	}
	leasedControl, err := value.LeaseNextRun(ctx, domain.LaneControl, time.Now(), time.Minute)
	if err != nil || leasedControl.ID != controlRun.ID {
		t.Fatalf("Lease control: run=%#v err=%v", leasedControl, err)
	}
	if err := value.MarkRunRunning(ctx, leasedControl.ID, leasedControl.LeaseToken); err != nil {
		t.Fatalf("Mark control running: %v", err)
	}
	requested, err := value.RequestSessionCancel(ctx, active.run.SessionID, leasedControl.ID)
	if err != nil {
		t.Fatalf("RequestSessionCancel: %v", err)
	}
	if len(requested) != 1 || requested[0] != active.run.ID {
		t.Fatalf("requested=%v, want active run only", requested)
	}
	storedActive, err := value.GetRun(ctx, active.run.ID)
	if err != nil || storedActive.State != domain.RunCancelRequested {
		t.Fatalf("active run=%#v err=%v", storedActive, err)
	}
	storedControl, err := value.GetRun(ctx, leasedControl.ID)
	if err != nil || storedControl.State != domain.RunRunning {
		t.Fatalf("control run=%#v err=%v", storedControl, err)
	}
	if err := value.MarkRunCancelled(ctx, active.run.ID, active.run.LeaseToken); err != nil {
		t.Fatalf("MarkRunCancelled: %v", err)
	}
	cancelledTurn, err := value.GetTurn(ctx, active.turn.ID)
	if err != nil || cancelledTurn.State != domain.TurnCancelled {
		t.Fatalf("cancelled turn=%#v err=%v", cancelledTurn, err)
	}
}
