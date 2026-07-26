package execution

import (
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

func TestRelayRetryIgnoresLegacyDeadlineButRetainsAttemptBudget(t *testing.T) {
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	now := time.Now()
	if !shouldRetryRun(manager.Current(), domain.Run{Attempt: 2, Deadline: now.Add(-time.Hour)}, now, errors.New("temporary")) {
		t.Fatal("legacy Relay deadline incorrectly disabled the normal retry budget")
	}
	if shouldRetryRun(manager.Current(), domain.Run{Attempt: 3, Deadline: now.Add(-time.Hour)}, now, errors.New("temporary")) {
		t.Fatal("Relay retry exceeded its existing attempt budget")
	}
}

func TestInvalidRequiredObserveIsNeverRetried(t *testing.T) {
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cause := errors.Join(errors.New("durable result failed"), agent.ErrInvalidRequiredObserve)
	if shouldRetryRun(manager.Current(), domain.Run{Attempt: 1}, time.Now(), cause) {
		t.Fatal("invalid required observe would rerun the entire Relay invocation")
	}
}
