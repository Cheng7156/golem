package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/routing"
	storeport "golem_plugin_hermes/internal/store"
)

type Processor struct {
	store         storeport.Store
	router        routing.Router
	reorderWindow time.Duration
	batchSize     int
	pollInterval  time.Duration
	wake          chan struct{}
	runWake       chan<- struct{}
	now           func() time.Time
	admitter      RunAdmitter
}

type RunAdmitter interface {
	AdmitRun(context.Context, domain.Run) (domain.RunAdmissionResult, error)
}

func NewProcessor(
	store storeport.Store,
	router routing.Router,
	reorderWindow time.Duration,
	runWake chan<- struct{},
	admitters ...RunAdmitter,
) (*Processor, error) {
	if store == nil || router == nil {
		return nil, errors.New("ingress processor 缺少 store 或 router")
	}
	processor := &Processor{
		store:         store,
		router:        router,
		reorderWindow: max(0, reorderWindow),
		batchSize:     256,
		pollInterval:  50 * time.Millisecond,
		wake:          make(chan struct{}, 1),
		runWake:       runWake,
		now:           time.Now,
	}
	if len(admitters) > 0 {
		processor.admitter = admitters[0]
	}
	return processor, nil
}

func (p *Processor) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Processor) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.wake:
		case <-timer.C:
		}
		worked, err := p.process(ctx)
		if err != nil {
			return err
		}
		delay := p.pollInterval
		if worked {
			delay = 0
		}
		timer.Reset(delay)
	}
}

func (p *Processor) process(ctx context.Context) (bool, error) {
	materialized, err := p.materializeAccepted(ctx)
	if err != nil {
		return false, err
	}
	routed, err := p.routeOrdered(ctx)
	if err != nil {
		return false, err
	}
	return materialized || routed, nil
}

func (p *Processor) materializeAccepted(ctx context.Context) (bool, error) {
	events, err := p.store.ListInbox(ctx, domain.InboxAccepted, p.batchSize)
	if err != nil {
		return false, err
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].SessionID != events[j].SessionID {
			return events[i].SessionID < events[j].SessionID
		}
		if !events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].OccurredAt.Before(events[j].OccurredAt)
		}
		if events[i].MessageID != events[j].MessageID {
			return events[i].MessageID < events[j].MessageID
		}
		return events[i].AcceptSeq < events[j].AcceptSeq
	})
	now := p.now()
	worked := false
	for _, event := range events {
		var message domain.InboundMessage
		explicit := json.Unmarshal(event.Payload, &message) == nil && message.Explicit()
		if !explicit && event.AcceptedAt.Add(p.reorderWindow).After(now) {
			continue
		}
		priority := 10
		if explicit {
			priority = 100
		}
		if _, err := p.store.MaterializeTurn(ctx, event.ID, priority); err != nil &&
			!errors.Is(err, storeport.ErrConflict) {
			return worked, err
		}
		worked = true
	}
	return worked, nil
}

func (p *Processor) routeOrdered(ctx context.Context) (bool, error) {
	turns, err := p.store.ListTurns(ctx, domain.TurnOrdered, p.batchSize)
	if err != nil {
		return false, err
	}
	worked := false
	for _, turn := range turns {
		event, err := p.store.GetInbox(ctx, turn.EventID)
		if err != nil {
			return worked, err
		}
		var message domain.InboundMessage
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			slog.Warn("[hermes] Inbox payload 无法解析，降级为观察", "event_id", event.ID, "err", err)
			if _, _, routeErr := p.store.RouteTurn(
				ctx, turn.ID, domain.RouteObserve, "", time.Time{},
			); routeErr != nil && !errors.Is(routeErr, storeport.ErrConflict) {
				return worked, routeErr
			}
			worked = true
			continue
		}
		decision, err := p.router.Route(ctx, event, message)
		if err != nil {
			return worked, err
		}
		routed, run, err := p.store.RouteTurnWithContextDisposition(
			ctx,
			turn.ID,
			decision.Route,
			decision.Lane,
			decision.Deadline,
			decision.Disposition == routing.DispositionIgnore,
		)
		if err != nil {
			if errors.Is(err, storeport.ErrConflict) {
				continue
			}
			return worked, err
		}
		admission := domain.RunAdmissionResult{}
		if run != nil && p.admitter != nil {
			admission, err = p.admitter.AdmitRun(ctx, *run)
			if err != nil {
				return worked, err
			}
		}
		slog.Debug("[hermes] Turn 已路由",
			"turn_id", routed.ID,
			"session_id", routed.SessionID,
			"route", routed.Route,
			"reason", decision.Reason,
		)
		if run != nil && !admission.CurrentSuperseded {
			notify(p.runWake)
		}
		worked = true
	}
	return worked, nil
}

func notify(channel chan<- struct{}) {
	if channel == nil {
		return
	}
	select {
	case channel <- struct{}{}:
	default:
	}
}
