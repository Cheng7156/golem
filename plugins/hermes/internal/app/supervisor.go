package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Runner interface {
	Run(context.Context) error
}

type RunnerFunc func(context.Context) error

func (f RunnerFunc) Run(ctx context.Context) error {
	return f(ctx)
}

type Supervisor struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
	errs    []error
}

func (s *Supervisor) Start(parent context.Context, runners ...Runner) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("supervisor 已启动")
	}
	for index, runner := range runners {
		if runner == nil {
			return fmt.Errorf("runner %d 为空", index)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.started = true

	var wg sync.WaitGroup
	for _, runner := range runners {
		wg.Add(1)
		go func(component Runner) {
			defer wg.Done()
			if err := component.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.record(err)
				cancel()
			}
		}(runner)
	}
	go func() {
		wg.Wait()
		close(s.done)
	}()
	return nil
}

func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	cancel()
	select {
	case <-done:
		return s.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) Wait(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return errors.New("supervisor 尚未启动")
	}
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		return s.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.errs...)
}

func (s *Supervisor) record(err error) {
	s.mu.Lock()
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}
