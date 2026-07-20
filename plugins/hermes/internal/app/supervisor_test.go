package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"golem_plugin_hermes/internal/app"
)

func TestSupervisorCancelsSiblingsWhenRunnerFails(t *testing.T) {
	t.Parallel()
	failure := errors.New("runner failed")
	var siblingCancelled atomic.Bool
	started := make(chan struct{})
	sibling := app.RunnerFunc(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		siblingCancelled.Store(true)
		return ctx.Err()
	})
	failing := app.RunnerFunc(func(ctx context.Context) error {
		<-started
		return failure
	})

	var supervisor app.Supervisor
	if err := supervisor.Start(context.Background(), sibling, failing); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := supervisor.Wait(ctx)
	if !errors.Is(err, failure) {
		t.Fatalf("Wait error=%v, want %v", err, failure)
	}
	if !siblingCancelled.Load() {
		t.Fatal("sibling runner did not receive cancellation")
	}
}

func TestSupervisorStopDrainsRunners(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	runner := app.RunnerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return nil
	})
	var supervisor app.Supervisor
	if err := supervisor.Start(context.Background(), runner); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("runner was not drained")
	}
}
