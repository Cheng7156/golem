package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type Kernel struct {
	store      storeport.Store
	runners    []Runner
	supervisor Supervisor

	mu      sync.Mutex
	started bool
}

func NewKernel(store storeport.Store, runners ...Runner) (*Kernel, error) {
	if store == nil {
		return nil, errors.New("kernel store 不能为空")
	}
	return &Kernel{store: store, runners: append([]Runner(nil), runners...)}, nil
}

func (k *Kernel) Start(ctx context.Context, now time.Time) (domain.RecoveryResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started {
		return domain.RecoveryResult{}, errors.New("kernel 已启动")
	}
	recovered, err := k.store.Recover(ctx, now)
	if err != nil {
		return domain.RecoveryResult{}, err
	}
	if err := k.supervisor.Start(context.WithoutCancel(ctx), k.runners...); err != nil {
		return domain.RecoveryResult{}, err
	}
	k.started = true
	return recovered, nil
}

func (k *Kernel) Stop(ctx context.Context) error {
	k.mu.Lock()
	started := k.started
	k.started = false
	k.mu.Unlock()
	if !started {
		return nil
	}
	return k.supervisor.Stop(ctx)
}
