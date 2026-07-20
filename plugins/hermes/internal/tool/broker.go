package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotRegistered          = errors.New("tool is not registered")
	ErrCapabilityDenied       = errors.New("tool capability denied")
	ErrSideEffectRequiresPlan = errors.New("side-effecting tool requires an effect proposal")
	ErrResultTooLarge         = errors.New("tool result exceeds size limit")
)

type Authorizer interface {
	Authorize(context.Context, Scope, Spec) error
}

type CapabilityAuthorizer struct{}

func (CapabilityAuthorizer) Authorize(_ context.Context, scope Scope, spec Spec) error {
	for _, capability := range spec.RequiredCapabilities {
		if !scope.Has(capability) {
			return fmt.Errorf("%w: %s", ErrCapabilityDenied, capability)
		}
	}
	return nil
}

type registeredTool struct {
	tool  Tool
	spec  Spec
	slots chan struct{}
}

type Broker struct {
	mu             sync.RWMutex
	tools          map[string]*registeredTool
	globalSlots    chan struct{}
	maxResultBytes int
	authorizer     Authorizer
}

func NewBroker(globalConcurrency, maxResultBytes int, authorizer Authorizer) (*Broker, error) {
	if globalConcurrency <= 0 || maxResultBytes <= 0 {
		return nil, errors.New("tool broker limits must be positive")
	}
	if authorizer == nil {
		authorizer = CapabilityAuthorizer{}
	}
	return &Broker{
		tools:          make(map[string]*registeredTool),
		globalSlots:    make(chan struct{}, globalConcurrency),
		maxResultBytes: maxResultBytes,
		authorizer:     authorizer,
	}, nil
}

func (b *Broker) Register(value Tool) error {
	if value == nil {
		return errors.New("cannot register a nil tool")
	}
	spec := value.Spec()
	if err := spec.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.tools[spec.Name]; exists {
		return fmt.Errorf("tool %q is already registered", spec.Name)
	}
	b.tools[spec.Name] = &registeredTool{
		tool:  value,
		spec:  spec,
		slots: make(chan struct{}, spec.ConcurrencyLimit),
	}
	return nil
}

func (b *Broker) Specs(ctx context.Context, scope Scope) []Spec {
	b.mu.RLock()
	names := make([]string, 0, len(b.tools))
	for name := range b.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]*registeredTool, 0, len(names))
	for _, name := range names {
		values = append(values, b.tools[name])
	}
	b.mu.RUnlock()
	result := make([]Spec, 0, len(values))
	for _, value := range values {
		if value.spec.SideEffect {
			continue
		}
		if b.authorizer.Authorize(ctx, scope, value.spec) == nil {
			result = append(result, value.spec)
		}
	}
	return result
}

func (b *Broker) Invoke(ctx context.Context, scope Scope, call Call) (Result, error) {
	if err := call.Validate(); err != nil {
		return Result{InvocationID: call.InvocationID}, err
	}
	b.mu.RLock()
	registered := b.tools[call.Name]
	b.mu.RUnlock()
	if registered == nil {
		return Result{InvocationID: call.InvocationID}, ErrNotRegistered
	}
	if registered.spec.SideEffect {
		return Result{InvocationID: call.InvocationID}, ErrSideEffectRequiresPlan
	}
	if err := b.authorizer.Authorize(ctx, scope, registered.spec); err != nil {
		return Result{InvocationID: call.InvocationID}, err
	}

	deadline := time.Now().Add(registered.spec.DefaultTimeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	if !call.Deadline.IsZero() && call.Deadline.Before(deadline) {
		deadline = call.Deadline
	}
	invokeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := acquire(invokeCtx, b.globalSlots); err != nil {
		return Result{InvocationID: call.InvocationID}, err
	}
	defer release(b.globalSlots)
	if err := acquire(invokeCtx, registered.slots); err != nil {
		return Result{InvocationID: call.InvocationID}, err
	}
	defer release(registered.slots)

	result, err := registered.tool.Invoke(invokeCtx, Invocation{Call: call, Scope: scope})
	if result.InvocationID == "" {
		result.InvocationID = call.InvocationID
	}
	if err != nil {
		return result, err
	}
	if len(result.Output) > 0 && !json.Valid(result.Output) {
		return result, errors.New("tool returned invalid JSON")
	}
	limit := registered.spec.MaxResultBytes
	if limit <= 0 || limit > b.maxResultBytes {
		limit = b.maxResultBytes
	}
	if len(result.Output) > limit {
		return Result{InvocationID: call.InvocationID}, ErrResultTooLarge
	}
	return result, nil
}

func acquire(ctx context.Context, slots chan struct{}) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(slots chan struct{}) {
	<-slots
}
