package observation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type Store interface {
	LeaseNextObservationBatch(context.Context, time.Time, time.Duration, int) (domain.ObservationBatch, error)
	MarkObservationBatchAcked(context.Context, domain.ObservationBatch, domain.ObservationAck) error
	MarkObservationBatchRetry(context.Context, domain.ObservationBatch, string, time.Time) error
	MarkObservationBatchTerminal(context.Context, domain.ObservationBatch, domain.ContextOutboxState, string) error
}

type Dispatcher struct {
	store         Store
	gateway       agent.ObservationGateway
	wake          <-chan struct{}
	now           func() time.Time
	pollInterval  time.Duration
	leaseDuration time.Duration
	batchSize     int
	enabled       func() bool
}

func NewDispatcher(store Store, gateway agent.ObservationGateway, wake <-chan struct{}) (*Dispatcher, error) {
	if store == nil || gateway == nil {
		return nil, errors.New("observation dispatcher requires store and gateway")
	}
	return &Dispatcher{store: store, gateway: gateway, wake: wake, now: time.Now, enabled: func() bool { return true },
		pollInterval: 100 * time.Millisecond, leaseDuration: 45 * time.Second, batchSize: 32}, nil
}

func (d *Dispatcher) SetEnabled(enabled func() bool) {
	if enabled != nil {
		d.enabled = enabled
	}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !d.enabled() || !d.gateway.SupportsObservationV2() {
			if err := d.wait(ctx); err != nil {
				return err
			}
			continue
		}
		batch, err := d.store.LeaseNextObservationBatch(ctx, d.now(), d.leaseDuration, d.batchSize)
		if errors.Is(err, storeport.ErrNotFound) {
			if err := d.wait(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		ack, sendErr := d.gateway.ObserveBatch(ctx, batch)
		if sendErr == nil {
			sendErr = d.store.MarkObservationBatchAcked(ctx, batch, ack)
		}
		if sendErr != nil {
			if errors.Is(sendErr, storeport.ErrConflict) || batch.Attempt >= 8 {
				state := domain.ContextConflict
				if batch.Attempt >= 8 && !errors.Is(sendErr, storeport.ErrConflict) {
					state = domain.ContextDeadLetter
				}
				if terminalErr := d.store.MarkObservationBatchTerminal(context.WithoutCancel(ctx), batch, state, sendErr.Error()); terminalErr != nil {
					return errors.Join(sendErr, terminalErr)
				}
				slog.Error("[hermes] observation batch entered terminal state", "batch_id", batch.BatchID,
					"state", state, "err", sendErr)
				continue
			}
			next := d.now().Add(time.Duration(1<<min(batchAttempt(batch), 6)) * time.Second)
			if retryErr := d.store.MarkObservationBatchRetry(context.WithoutCancel(ctx), batch, sendErr.Error(), next); retryErr != nil {
				return errors.Join(sendErr, retryErr)
			}
			slog.Warn("[hermes] observation batch retry scheduled", "batch_id", batch.BatchID, "err", sendErr)
		}
	}
}

func batchAttempt(batch domain.ObservationBatch) int { return max(1, batch.Attempt) }

func (d *Dispatcher) wait(ctx context.Context) error {
	timer := time.NewTimer(d.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.wake:
		return nil
	case <-timer.C:
		return nil
	}
}
