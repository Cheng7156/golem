package cancellation

import (
	"context"
	"errors"
	"fmt"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type Controller struct {
	Store     storeport.Store
	Canceller agent.Canceller
}

func (c Controller) Interrupt(ctx context.Context, runID string) error {
	if c.Store == nil {
		return errors.New("run cancellation requires a store")
	}
	run, err := c.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State == domain.RunCancelled {
		return nil
	}
	if run.State != domain.RunCancelRequested {
		return fmt.Errorf("run %s cannot be interrupted from state %s", runID, run.State)
	}
	if c.Canceller == nil {
		return errors.New("Hermes engine does not support active run cancellation")
	}
	err = c.Canceller.CancelRun(ctx, runID)
	if !errors.Is(err, agent.ErrRunNotActive) {
		return err
	}
	return c.finalizeInactiveRun(ctx, run)
}

func (c Controller) finalizeInactiveRun(ctx context.Context, run domain.Run) error {
	err := c.Store.MarkRunCancelled(ctx, run.ID, run.LeaseToken)
	if err == nil || !errors.Is(err, storeport.ErrConflict) {
		return err
	}
	current, getErr := c.Store.GetRun(ctx, run.ID)
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	if current.State == domain.RunCancelled {
		return nil
	}
	return fmt.Errorf("finalize inactive run %s in state %s: %w", run.ID, current.State, err)
}
