package output

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const (
	defaultOutboxLeaseDuration = 30 * time.Second
	outboxLeaseSafetyMargin    = 30 * time.Second
	outboxStateWriteTimeout    = 5 * time.Second
)

type Receipt struct {
	ID        uint64
	CreatedAt time.Time
}

type Sender interface {
	Send(context.Context, domain.OutboxItem) (Receipt, error)
}

type AmbiguousError struct {
	Err error
}

type PermanentError struct {
	Err error
}

func (e AmbiguousError) Error() string {
	if e.Err == nil {
		return "发送结果不确定"
	}
	return e.Err.Error()
}

func (e AmbiguousError) Unwrap() error {
	return e.Err
}

func (e PermanentError) Error() string {
	if e.Err == nil {
		return "permanent delivery failure"
	}
	return e.Err.Error()
}

func (e PermanentError) Unwrap() error {
	return e.Err
}

type Dispatcher struct {
	store         storeport.Store
	sender        Sender
	config        func() *config.Snapshot
	wake          <-chan struct{}
	pollInterval  time.Duration
	leaseDuration time.Duration
	now           func() time.Time
	throttler     *Throttler
}

type deliveryFailure struct {
	item   domain.OutboxItem
	config config.OutputConfig
	err    error
}

func NewDispatcher(
	store storeport.Store,
	sender Sender,
	snapshot func() *config.Snapshot,
	wake <-chan struct{},
	throttler ...*Throttler,
) (*Dispatcher, error) {
	if store == nil || sender == nil || snapshot == nil {
		return nil, errors.New("output dispatcher 缺少 store、sender 或 config")
	}
	return &Dispatcher{
		store:         store,
		sender:        sender,
		config:        snapshot,
		wake:          wake,
		pollInterval:  50 * time.Millisecond,
		leaseDuration: defaultOutboxLeaseDuration,
		now:           time.Now,
		throttler:     firstThrottler(throttler),
	}, nil
}

func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cfg := d.config()
		item, err := d.store.LeaseNextOutbox(ctx, d.now(), d.deliveryLeaseDuration(cfg))
		if errors.Is(err, storeport.ErrNotFound) {
			if err := d.wait(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		d.deliver(ctx, item, cfg)
	}
}

func (d *Dispatcher) deliveryLeaseDuration(cfg *config.Snapshot) time.Duration {
	result := d.leaseDuration
	if result <= 0 {
		result = defaultOutboxLeaseDuration
	}
	if cfg == nil || cfg.Output.SendTimeoutSeconds <= 0 {
		return result
	}
	sendTimeout := time.Duration(cfg.Output.SendTimeoutSeconds) * time.Second
	sendInterval := time.Duration(cfg.Output.SendIntervalMilliseconds) * time.Millisecond
	required := sendTimeout + sendInterval + outboxLeaseSafetyMargin
	if required > result {
		return required
	}
	return result
}

func (d *Dispatcher) deliver(
	parent context.Context,
	item domain.OutboxItem,
	cfg *config.Snapshot,
) {
	if cfg == nil {
		stateCtx, cancel := outboxStateContext(parent)
		defer cancel()
		d.recordRetry(stateCtx, item, "failed", "output config unavailable", d.now().Add(time.Second))
		return
	}
	if err := d.waitSendInterval(parent, item.ReceiverID, cfg.Output.SendIntervalMilliseconds); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.Output.SendTimeoutSeconds)*time.Second)
	receipt, err := d.sender.Send(ctx, item)
	cancel()
	err = validateReceipt(receipt, err)
	stateCtx, stateCancel := outboxStateContext(parent)
	defer stateCancel()
	if err == nil {
		d.recordSent(stateCtx, item, receipt)
		return
	}
	d.handleFailure(stateCtx, deliveryFailure{item: item, config: cfg.Output, err: err})
}

func (d *Dispatcher) handleFailure(parent context.Context, failure deliveryFailure) {
	outcome, maxAttempts, permanent := failurePolicy(failure.err, failure.config)
	if failure.item.Kind == "video" {
		d.recordDeadLetter(
			parent, failure.item, outcome,
			"video delivery failed; automatic retry suppressed: "+failure.err.Error(),
		)
		return
	}
	if permanent || failure.item.Attempt >= maxAttempts {
		d.recordDeadLetter(parent, failure.item, outcome, failure.err.Error())
		return
	}
	delay := retryDelay(failure.config, failure.item.Attempt)
	d.recordRetry(parent, failure.item, outcome, failure.err.Error(), d.now().Add(delay))
}

func outboxStateContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), outboxStateWriteTimeout)
}

func (d *Dispatcher) recordSent(ctx context.Context, item domain.OutboxItem, receipt Receipt) {
	if err := d.store.MarkOutboxSent(ctx, item.ID, item.LeaseToken, receipt.ID, receipt.CreatedAt); err != nil {
		slog.Error("[hermes] persist sent outbox failed", "outbox_id", item.ID, "err", err)
	}
}

func (d *Dispatcher) recordDeadLetter(
	ctx context.Context,
	item domain.OutboxItem,
	outcome string,
	message string,
) {
	if err := d.store.MarkOutboxDeadLetter(ctx, item.ID, item.LeaseToken, outcome, message); err != nil {
		slog.Error("[hermes] persist dead-letter outbox failed", "outbox_id", item.ID, "err", err)
	}
}

func (d *Dispatcher) recordRetry(
	ctx context.Context,
	item domain.OutboxItem,
	outcome string,
	message string,
	nextAttempt time.Time,
) {
	if err := d.store.MarkOutboxRetry(ctx, item.ID, item.LeaseToken, outcome, message, nextAttempt); err != nil {
		slog.Error("[hermes] persist retry outbox failed", "outbox_id", item.ID, "err", err)
	}
}

func failurePolicy(err error, cfg config.OutputConfig) (string, int, bool) {
	var permanent PermanentError
	if errors.As(err, &permanent) {
		return "failed", 1, true
	}
	var ambiguous AmbiguousError
	if errors.As(err, &ambiguous) {
		return "ambiguous", cfg.AmbiguousMaxAttempts, false
	}
	return "failed", cfg.MaxAttempts, false
}

func validateReceipt(receipt Receipt, sendErr error) error {
	if sendErr != nil {
		return sendErr
	}
	if receipt.ID != 0 {
		return nil
	}
	return AmbiguousError{Err: errors.New("sender returned a zero receipt id")}
}

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

func (d *Dispatcher) waitSendInterval(ctx context.Context, receiverID string, milliseconds int) error {
	if d.throttler == nil || milliseconds <= 0 {
		return nil
	}
	interval := time.Duration(milliseconds) * time.Millisecond
	return d.throttler.Wait(ctx, receiverID, interval, d.now)
}
