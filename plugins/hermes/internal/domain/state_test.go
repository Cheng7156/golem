package domain_test

import (
	"testing"

	"golem_plugin_hermes/internal/domain"
)

func TestTurnTransitionGraphRejectsSkippedCommit(t *testing.T) {
	t.Parallel()
	if err := domain.ValidateTurnTransition(domain.TurnRunning, domain.TurnCompleted); err == nil {
		t.Fatal("running must not skip committing")
	}
	if err := domain.ValidateTurnTransition(domain.TurnRunning, domain.TurnCommitting); err != nil {
		t.Fatalf("running -> committing: %v", err)
	}
	if err := domain.ValidateTurnTransition(domain.TurnCommitting, domain.TurnCompleted); err != nil {
		t.Fatalf("committing -> completed: %v", err)
	}
}

func TestRunTransitionGraphSupportsCooperativeCancellation(t *testing.T) {
	t.Parallel()
	if err := domain.ValidateRunTransition(domain.RunRunning, domain.RunCancelRequested); err != nil {
		t.Fatalf("running -> cancel_requested: %v", err)
	}
	if err := domain.ValidateRunTransition(domain.RunCancelRequested, domain.RunCancelled); err != nil {
		t.Fatalf("cancel_requested -> cancelled: %v", err)
	}
	if err := domain.ValidateRunTransition(domain.RunSucceeded, domain.RunRetryWait); err == nil {
		t.Fatal("succeeded run must be terminal")
	}
}
