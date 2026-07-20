package main

import (
	"context"
	"testing"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/domain"
	sqlitestore "golem_plugin_hermes/internal/store/sqlite"
)

type recordingCommandCanceller struct {
	agent.Engine
	runIDs []string
	err    error
}

func (c *recordingCommandCanceller) CancelRun(_ context.Context, runID string) error {
	c.runIDs = append(c.runIDs, runID)
	return c.err
}

func TestHermesResetPreemptsRunningAndQueuedSessionRuns(t *testing.T) {
	p, store := newCommandTestPlugin(t)
	for range 2 {
		if _, err := p.OnCommand(privateHermesCommand("status")); err != nil {
			t.Fatalf("OnCommand(status): %v", err)
		}
	}
	events := acceptedCommandEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("accepted status events=%d, want 2", len(events))
	}
	runs := []domain.Run{
		routeCommandRun(t, store, events[0]),
		routeCommandRun(t, store, events[1]),
	}
	leased, err := store.LeaseNextRun(
		context.Background(), domain.LaneInteractive, time.Now(), time.Minute,
	)
	if err != nil {
		t.Fatalf("LeaseNextRun: %v", err)
	}
	if err := store.MarkRunRunning(context.Background(), leased.ID, leased.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	canceller := &recordingCommandCanceller{}
	p.engine = canceller

	if result, err := p.OnCommand(privateHermesCommand("reset")); err != nil || result != "" {
		t.Fatalf("OnCommand(reset)=%q, %v", result, err)
	}
	assertPreemptedCommandRuns(t, store, runs, leased.ID)
	if len(canceller.runIDs) != 1 || canceller.runIDs[0] != leased.ID {
		t.Fatalf("cancelled runs=%v, want [%s]", canceller.runIDs, leased.ID)
	}
	resetEvents := acceptedCommandEvents(t, store)
	if len(resetEvents) != 1 || decodeCommandMessage(t, resetEvents[0]).HermesCommand != "/reset" {
		t.Fatalf("accepted reset events=%#v", resetEvents)
	}
}

func TestOnlySessionBoundaryCommandsPreempt(t *testing.T) {
	for command, want := range map[string]bool{
		"/new": true, "/reset": true, "/status": false, "/cancel": false,
	} {
		if actual := preemptsHermesSession(command); actual != want {
			t.Errorf("preemptsHermesSession(%q)=%v, want %v", command, actual, want)
		}
	}
}

func TestHermesResetFinalizesRunMissingFromEngine(t *testing.T) {
	p, store := newCommandTestPlugin(t)
	if _, err := p.OnCommand(privateHermesCommand("status")); err != nil {
		t.Fatalf("OnCommand(status): %v", err)
	}
	events := acceptedCommandEvents(t, store)
	run := routeCommandRun(t, store, events[0])
	leased, err := store.LeaseNextRun(
		context.Background(), domain.LaneInteractive, time.Now(), time.Minute,
	)
	if err != nil {
		t.Fatalf("LeaseNextRun: %v", err)
	}
	if err := store.MarkRunRunning(context.Background(), leased.ID, leased.LeaseToken); err != nil {
		t.Fatalf("MarkRunRunning: %v", err)
	}
	p.engine = &recordingCommandCanceller{err: agent.ErrRunNotActive}

	if _, err := p.OnCommand(privateHermesCommand("reset")); err != nil {
		t.Fatalf("OnCommand(reset): %v", err)
	}
	stored, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.State != domain.RunCancelled || stored.LeaseToken != "" {
		t.Fatalf("run after reset=%#v, want terminal cancellation", stored)
	}
}

func routeCommandRun(
	t *testing.T,
	store *sqlitestore.Store,
	event domain.InboxEvent,
) domain.Run {
	t.Helper()
	turn, err := store.MaterializeTurn(context.Background(), event.ID, 100)
	if err != nil {
		t.Fatalf("MaterializeTurn: %v", err)
	}
	_, run, err := store.RouteTurn(
		context.Background(), turn.ID, domain.RouteChat,
		domain.LaneInteractive, time.Time{},
	)
	if err != nil {
		t.Fatalf("RouteTurn: %v", err)
	}
	return *run
}

func assertPreemptedCommandRuns(
	t *testing.T,
	store *sqlitestore.Store,
	runs []domain.Run,
	runningID string,
) {
	t.Helper()
	for _, run := range runs {
		stored, err := store.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", run.ID, err)
		}
		want := domain.RunCancelled
		if run.ID == runningID {
			want = domain.RunCancelRequested
		}
		if stored.State != want {
			t.Errorf("run %s state=%s, want %s", run.ID, stored.State, want)
		}
	}
}
