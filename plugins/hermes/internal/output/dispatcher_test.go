package output

import (
	"context"
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func TestValidateReceiptRejectsZeroIDAsAmbiguous(t *testing.T) {
	err := validateReceipt(Receipt{}, nil)
	var ambiguous AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error=%v, want AmbiguousError", err)
	}
	if err := validateReceipt(Receipt{ID: 1}, nil); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
}

func TestFailurePolicyDoesNotRetryPermanentFailures(t *testing.T) {
	outcome, maxAttempts, permanent := failurePolicy(
		PermanentError{Err: errors.New("rejected")}, config.Default().Output,
	)
	if outcome != "failed" || maxAttempts != 1 || !permanent {
		t.Fatalf("policy=(%s,%d,%t)", outcome, maxAttempts, permanent)
	}
}

func TestFailurePolicyBoundsAmbiguousRetries(t *testing.T) {
	cfg := config.Default().Output
	outcome, maxAttempts, permanent := failurePolicy(
		AmbiguousError{Err: errors.New("receipt lost")}, cfg,
	)
	if outcome != "ambiguous" || maxAttempts != 2 || permanent {
		t.Fatalf("policy=(%s,%d,%t)", outcome, maxAttempts, permanent)
	}
}

func TestThrottlerReservesPerReceiverInterval(t *testing.T) {
	throttler := NewThrottler()
	now := time.Unix(100, 0)
	clock := func() time.Time { return now }
	interval := 700 * time.Millisecond

	if delay := throttler.reserveDelay("room-1", interval, clock); delay != 0 {
		t.Fatalf("first room-1 delay=%s, want 0", delay)
	}
	if delay := throttler.reserveDelay("room-1", interval, clock); delay != interval {
		t.Fatalf("second room-1 delay=%s, want %s", delay, interval)
	}
	if delay := throttler.reserveDelay("room-2", interval, clock); delay != 0 {
		t.Fatalf("first room-2 delay=%s, want 0", delay)
	}

	now = now.Add(200 * time.Millisecond)
	if delay := throttler.reserveDelay("room-1", interval, clock); delay != 500*time.Millisecond {
		t.Fatalf("early room-1 delay=%s, want 500ms", delay)
	}

	now = now.Add(500 * time.Millisecond)
	if delay := throttler.reserveDelay("room-1", interval, clock); delay != 0 {
		t.Fatalf("ready room-1 delay=%s, want 0", delay)
	}
}

type recordingStore struct {
	storeport.Store
	retryCalled     bool
	deadCalled      bool
	ambiguousCalled bool
	sendingCalled   bool
	outcome         string
	message         string
	contextErr      error
}

func (s *recordingStore) MarkOutboxSending(context.Context, string, string) error {
	s.sendingCalled = true
	return nil
}

func (s *recordingStore) MarkOutboxRetry(
	ctx context.Context,
	_ string,
	_ string,
	_ string,
	_ string,
	_ time.Time,
) error {
	s.retryCalled = true
	s.contextErr = ctx.Err()
	return nil
}

func (s *recordingStore) MarkOutboxAmbiguous(
	ctx context.Context,
	_ string,
	_ string,
	message string,
) error {
	s.ambiguousCalled = true
	s.message = message
	s.contextErr = ctx.Err()
	return nil
}

func (s *recordingStore) MarkOutboxDeadLetter(
	ctx context.Context,
	_ string,
	_ string,
	outcome string,
	message string,
) error {
	s.deadCalled = true
	s.outcome = outcome
	s.message = message
	s.contextErr = ctx.Err()
	return nil
}

type failingSender struct{ err error }

func (s failingSender) Send(context.Context, domain.OutboxItem) (Receipt, error) {
	return Receipt{}, s.err
}

func TestVideoDeliveryRetriesOnlyWhenSendDidNotStart(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "not started", err: NotStartedError{Err: context.DeadlineExceeded}, want: "retry"},
		{name: "ambiguous", err: AmbiguousError{Err: context.DeadlineExceeded}, want: "ambiguous"},
		{name: "permanent", err: PermanentError{Err: errors.New("host rejected video")}, want: "dead"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := new(recordingStore)
			dispatcher := testDispatcher(store, test.err)
			dispatcher.deliver(context.Background(), videoOutbox(), dispatcher.config())
			switch test.want {
			case "retry":
				if !store.retryCalled || store.deadCalled || store.ambiguousCalled {
					t.Fatalf("store=%#v", store)
				}
			case "ambiguous":
				if !store.ambiguousCalled || store.retryCalled || store.deadCalled {
					t.Fatalf("store=%#v", store)
				}
			case "dead":
				if !store.deadCalled || store.retryCalled || store.ambiguousCalled {
					t.Fatalf("store=%#v", store)
				}
			}
		})
	}
}

func TestOtherDeliveryFailuresKeepExistingRetryPolicy(t *testing.T) {
	tests := []struct {
		name string
		kind string
		err  error
	}{
		{name: "ambiguous text", kind: "text", err: AmbiguousError{Err: context.DeadlineExceeded}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := new(recordingStore)
			dispatcher := testDispatcher(store, test.err)

			dispatcher.deliver(context.Background(), domain.OutboxItem{
				ID: "outbox", Kind: test.kind, LeaseToken: "lease", Attempt: 1,
			}, dispatcher.config())

			if !store.retryCalled || store.deadCalled {
				t.Fatalf("retry=%v dead=%v", store.retryCalled, store.deadCalled)
			}
		})
	}
}

func TestVideoNotStartedRetriesAreBounded(t *testing.T) {
	store := new(recordingStore)
	dispatcher := testDispatcher(store, NotStartedError{Err: context.DeadlineExceeded})
	item := videoOutbox()
	item.Attempt = dispatcher.config().Output.MaxAttempts
	dispatcher.deliver(context.Background(), item, dispatcher.config())
	if !store.deadCalled || store.retryCalled || store.ambiguousCalled || store.outcome != "not_started" {
		t.Fatalf("store=%#v", store)
	}
}

func TestDeliveryLeaseCoversConfiguredSendTimeout(t *testing.T) {
	dispatcher := testDispatcher(new(recordingStore), nil)
	dispatcher.leaseDuration = defaultOutboxLeaseDuration
	cfg := dispatcher.config()
	cfg.Output.SendTimeoutSeconds = 180
	cfg.Output.SendIntervalMilliseconds = 5000
	if got, want := dispatcher.deliveryLeaseDuration(cfg), 215*time.Second; got != want {
		t.Fatalf("lease duration=%s, want %s", got, want)
	}
}

func TestCancelledDeliveryStillPersistsVideoAmbiguousState(t *testing.T) {
	store := new(recordingStore)
	dispatcher := testDispatcher(store, context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dispatcher.deliver(ctx, videoOutbox(), dispatcher.config())
	if !store.ambiguousCalled || store.contextErr != nil {
		t.Fatalf("ambiguous=%v context_error=%v", store.ambiguousCalled, store.contextErr)
	}
}

func videoOutbox() domain.OutboxItem {
	return domain.OutboxItem{
		ID: "video-outbox", Kind: "video", LeaseToken: "lease", Attempt: 1,
	}
}

func testDispatcher(store storeport.Store, sendErr error) *Dispatcher {
	return &Dispatcher{
		store:  store,
		sender: failingSender{err: sendErr},
		config: func() *config.Snapshot {
			return &config.Snapshot{Config: config.Config{Output: config.OutputConfig{
				MaxAttempts: 12, AmbiguousMaxAttempts: 2, SendTimeoutSeconds: 1,
				RetryMinSeconds: 1, RetryMaxSeconds: 1,
			}}}
		},
		now: time.Now,
	}
}
