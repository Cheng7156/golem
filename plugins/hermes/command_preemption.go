package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/cancellation"
)

const hermesCommandControlTimeout = 5 * time.Second

func preemptsHermesSession(command string) bool {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "/new", "/reset":
		return true
	default:
		return false
	}
}

func (p *HermesPlugin) preemptHermesSession(sessionID string) error {
	p.lifecycleMu.Lock()
	store, engine := p.store, p.engine
	p.lifecycleMu.Unlock()
	if store == nil {
		return errors.New("Hermes Kernel 尚未启动")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hermesCommandControlTimeout)
	defer cancel()
	runIDs, err := store.RequestSessionCancel(ctx, sessionID, "")
	if err != nil {
		return fmt.Errorf("取消旧 Hermes 会话任务: %w", err)
	}
	var canceller agent.Canceller
	if engine != nil {
		canceller, _ = engine.(agent.Canceller)
	}
	controller := cancellation.Controller{Store: store, Canceller: canceller}
	return interruptHermesRuns(ctx, controller, runIDs)
}

func interruptHermesRuns(
	ctx context.Context,
	controller cancellation.Controller,
	runIDs []string,
) error {
	var result error
	for _, runID := range runIDs {
		if err := controller.Interrupt(ctx, runID); err != nil {
			result = errors.Join(result, fmt.Errorf("中断 Hermes Run %s: %w", runID, err))
		}
	}
	return result
}
