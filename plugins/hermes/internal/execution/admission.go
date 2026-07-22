package execution

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/cancellation"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type admissionStore interface {
	ReconcileRunAdmission(context.Context, string, domain.RunAdmissionMode) (domain.RunAdmissionResult, error)
}

type RunAdmissionCoordinator struct {
	store            storeport.Store
	admission        admissionStore
	canceller        agent.Canceller
	config           func() *config.Snapshot
	interruptTimeout time.Duration
}

func NewRunAdmissionCoordinator(
	store storeport.Store,
	engine agent.Engine,
	snapshot func() *config.Snapshot,
) (*RunAdmissionCoordinator, error) {
	admission, ok := store.(admissionStore)
	if store == nil || engine == nil || snapshot == nil || !ok {
		return nil, errors.New("run admission requires a compatible store and engine")
	}
	canceller, _ := engine.(agent.Canceller)
	return &RunAdmissionCoordinator{
		store: store, admission: admission, canceller: canceller, config: snapshot,
		interruptTimeout: 3 * time.Second,
	}, nil
}

func (c *RunAdmissionCoordinator) AdmitRun(
	ctx context.Context,
	run domain.Run,
) (domain.RunAdmissionResult, error) {
	mode := domain.RunAdmissionActive
	if cfg := c.config(); cfg != nil {
		mode = domain.RunAdmissionMode(cfg.Scheduler.RunAdmissionMode)
	}
	result, err := c.admission.ReconcileRunAdmission(ctx, run.ID, mode)
	if err != nil {
		return result, err
	}
	if len(result.CancelledRunIDs) > 0 || result.CurrentSuperseded {
		slog.Info("[hermes] Run admission superseded ambient work",
			"run_id", run.ID,
			"trigger_kind", run.TriggerKind,
			"current_superseded", result.CurrentSuperseded,
			"cancelled_runs", len(result.CancelledRunIDs),
			"interrupt_runs", len(result.InterruptRunIDs),
		)
	}
	if len(result.InterruptRunIDs) == 0 {
		return result, nil
	}
	if c.canceller == nil {
		slog.Warn("[hermes] active ambient Run cannot be interrupted by the current engine",
			"run_id", run.ID, "interrupt_runs", len(result.InterruptRunIDs))
		return result, nil
	}
	controller := cancellation.Controller{Store: c.store, Canceller: c.canceller}
	for _, runID := range result.InterruptRunIDs {
		interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.interruptTimeout)
		interruptErr := controller.Interrupt(interruptCtx, runID)
		cancel()
		if interruptErr != nil {
			slog.Warn("[hermes] failed to interrupt preempted ambient Run",
				"run_id", runID, "foreground_run_id", run.ID, "err", interruptErr)
		}
	}
	return result, nil
}
