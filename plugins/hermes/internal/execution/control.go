package execution

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/cancellation"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type ControlWorker struct {
	store         storeport.Store
	engine        agent.Engine
	wake          <-chan struct{}
	outputWake    chan<- struct{}
	pollInterval  time.Duration
	leaseDuration time.Duration
	now           func() time.Time
}

func NewControlWorker(
	store storeport.Store,
	engine agent.Engine,
	wake <-chan struct{},
	outputWake chan<- struct{},
) (*ControlWorker, error) {
	if store == nil || engine == nil {
		return nil, errors.New("control worker requires store and engine")
	}
	return &ControlWorker{
		store:         store,
		engine:        engine,
		wake:          wake,
		outputWake:    outputWake,
		pollInterval:  50 * time.Millisecond,
		leaseDuration: 15 * time.Second,
		now:           time.Now,
	}, nil
}

func (w *ControlWorker) Run(ctx context.Context) error {
	for {
		run, err := w.store.LeaseNextRun(ctx, domain.LaneControl, w.now(), w.leaseDuration)
		if errors.Is(err, storeport.ErrNotFound) {
			if err := w.wait(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := w.execute(ctx, run); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("[hermes] control run failed", "run_id", run.ID, "err", err)
		}
	}
}

func (w *ControlWorker) wait(ctx context.Context) error {
	timer := time.NewTimer(w.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (w *ControlWorker) execute(ctx context.Context, run domain.Run) error {
	if err := w.store.MarkRunRunning(ctx, run.ID, run.LeaseToken); err != nil {
		return err
	}
	turn, err := w.store.GetTurn(ctx, run.TurnID)
	if err != nil {
		return w.fail(ctx, run, err)
	}
	inbox, err := w.store.GetInbox(ctx, turn.EventID)
	if err != nil {
		return w.fail(ctx, run, err)
	}
	requested, err := w.store.RequestSessionCancel(ctx, run.SessionID, run.ID)
	if err != nil {
		return w.fail(ctx, run, err)
	}
	w.interruptRuns(ctx, requested)
	content := "No active Hermes runs were found."
	if len(requested) > 0 {
		content = "Cancellation requested."
	}
	payload, err := json.Marshal(domain.TextOutput{Content: content})
	if err != nil {
		return w.fail(ctx, run, err)
	}
	_, err = w.store.CommitRunSuccess(ctx, run.ID, run.LeaseToken, []domain.OutboxDraft{{
		SessionID:  run.SessionID,
		ReceiverID: inbox.Binding.ReceiverID,
		Kind:       "text",
		Payload:    payload,
	}})
	if err != nil {
		return err
	}
	signal(w.outputWake)
	return nil
}

func (w *ControlWorker) interruptRuns(ctx context.Context, runIDs []string) {
	canceller, _ := w.engine.(agent.Canceller)
	controller := cancellation.Controller{Store: w.store, Canceller: canceller}
	for _, runID := range runIDs {
		if err := controller.Interrupt(ctx, runID); err != nil {
			slog.Warn("[hermes] failed to interrupt cancelled run", "run_id", runID, "err", err)
		}
	}
}

func (w *ControlWorker) fail(ctx context.Context, run domain.Run, cause error) error {
	if err := w.store.FailRun(ctx, run.ID, run.LeaseToken, cause.Error(), false, time.Time{}); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
