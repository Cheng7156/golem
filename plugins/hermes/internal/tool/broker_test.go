package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type testTool struct {
	spec   Spec
	invoke func(context.Context, Invocation) (Result, error)
}

func (t testTool) Spec() Spec { return t.spec }

func (t testTool) Invoke(ctx context.Context, invocation Invocation) (Result, error) {
	return t.invoke(ctx, invocation)
}

func testSpec(name string) Spec {
	return Spec{
		Name:                 name,
		Version:              "1",
		InputSchema:          json.RawMessage(`{"type":"object"}`),
		RequiredCapabilities: []Capability{"history.read.current_session"},
		ReadOnly:             true,
		DefaultTimeout:       time.Second,
		ConcurrencyLimit:     1,
		MaxResultBytes:       128,
	}
}

func TestBrokerEnforcesCapabilitiesAndResultLimit(t *testing.T) {
	broker, err := NewBroker(2, 64, nil)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	toolValue := testTool{spec: testSpec("history"), invoke: func(context.Context, Invocation) (Result, error) {
		return Result{Output: json.RawMessage(`{"ok":true}`)}, nil
	}}
	if err := broker.Register(toolValue); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if specs := broker.Specs(context.Background(), Scope{}); len(specs) != 0 {
		t.Fatalf("unauthorized specs leaked: %#v", specs)
	}
	call := Call{InvocationID: "call-1", Name: "history", Arguments: json.RawMessage(`{}`)}
	if _, err := broker.Invoke(context.Background(), Scope{}, call); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("Invoke error=%v, want capability denied", err)
	}
	allowed := Scope{Capabilities: []Capability{"history.read.current_session"}}
	result, err := broker.Invoke(context.Background(), allowed, call)
	if err != nil || string(result.Output) != `{"ok":true}` {
		t.Fatalf("allowed Invoke result=%s err=%v", result.Output, err)
	}
}

func TestBrokerBoundsPerToolConcurrency(t *testing.T) {
	broker, _ := NewBroker(2, 1024, nil)
	entered := make(chan struct{})
	releaseFirst := make(chan struct{})
	value := testTool{spec: testSpec("bounded"), invoke: func(ctx context.Context, invocation Invocation) (Result, error) {
		close(entered)
		select {
		case <-releaseFirst:
			return Result{Output: json.RawMessage(`{}`)}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	if err := broker.Register(value); err != nil {
		t.Fatalf("Register: %v", err)
	}
	scope := Scope{Capabilities: []Capability{"history.read.current_session"}}
	firstDone := make(chan error, 1)
	go func() {
		_, err := broker.Invoke(context.Background(), scope, Call{
			InvocationID: "first", Name: "bounded", Arguments: json.RawMessage(`{}`),
		})
		firstDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := broker.Invoke(ctx, scope, Call{
		InvocationID: "second", Name: "bounded", Arguments: json.RawMessage(`{}`),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Invoke error=%v, want deadline exceeded", err)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
}
