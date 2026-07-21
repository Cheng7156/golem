package agent

import (
	"context"
	"errors"
	"testing"
)

func TestRunTerminatedFailureAndCancellationReleasePendingRun(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		message    string
		wantCancel bool
	}{
		{name: "failed", status: "failed", message: "agent crashed"},
		{name: "cancelled", status: "cancelled", wantCancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway, err := NewRelayGateway(RelayConfig{})
			if err != nil {
				t.Fatalf("NewRelayGateway: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			run := &relayRun{engine: gateway, request: RunRequest{RunID: "run-1", InvocationID: "invoke-1"},
				chatID: "chat-1", events: make(chan Event, 2), cancel: cancel}
			gateway.pending[run.chatID] = run

			if err := gateway.acceptRunTerminated(runTerminated{InvocationID: "invoke-1",
				ProposalID: "proposal-1", Status: test.status, Error: test.message}); err != nil {
				t.Fatalf("acceptRunTerminated: %v", err)
			}
			if _, exists := gateway.pending[run.chatID]; exists {
				t.Fatal("terminal Run remained pending")
			}
			if ctx.Err() == nil {
				t.Fatal("terminal Run context was not cancelled")
			}
			event, ok := <-run.events
			if !ok || event.Kind != EventRunFailed || event.InvocationID != "invoke-1" || event.ProposalID != "proposal-1" {
				t.Fatalf("terminal event=%#v ok=%v", event, ok)
			}
			if test.wantCancel {
				if !errors.Is(event.Err, context.Canceled) {
					t.Fatalf("event error=%v, want context.Canceled", event.Err)
				}
			} else if event.Err == nil || event.Err.Error() != test.message {
				t.Fatalf("event error=%v, want %q", event.Err, test.message)
			}
			if _, open := <-run.events; open {
				t.Fatal("terminal event stream remained open")
			}
		})
	}
}

func TestRunTerminatedCompletedIsConsistencyOnly(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &relayRun{engine: gateway, request: RunRequest{RunID: "run-1", InvocationID: "invoke-1"},
		chatID: "chat-1", events: make(chan Event, 2), cancel: cancel}
	gateway.pending[run.chatID] = run

	if err := gateway.acceptRunTerminated(runTerminated{InvocationID: "invoke-1",
		ProposalID: "proposal-1", Status: "completed"}); err != nil {
		t.Fatalf("acceptRunTerminated: %v", err)
	}
	if gateway.pending[run.chatID] != run {
		t.Fatal("completed consistency signal removed active Run before durable receipt")
	}
	select {
	case event := <-run.events:
		t.Fatalf("completed consistency signal emitted terminal event: %#v", event)
	default:
	}
}

func TestRunTerminatedWireUsesTerminalState(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	run := &relayRun{engine: gateway, request: RunRequest{RunID: "run-1", InvocationID: "invoke-wire"},
		chatID: "chat-1", events: make(chan Event, 2), cancel: cancel}
	gateway.pending[run.chatID] = run
	connection := &relayConnection{negotiated: true, v2: true}

	if err := gateway.handleFrame(context.Background(), connection,
		[]byte(`{"type":"run_terminated_v1","invocation_id":"invoke-wire","terminal_state":"failed","error":"wire failure"}`)); err != nil {
		t.Fatalf("handleFrame: %v", err)
	}
	if _, exists := gateway.pending[run.chatID]; exists {
		t.Fatal("terminal_state wire frame did not release pending Run")
	}
	event := <-run.events
	if event.Err == nil || event.Err.Error() != "wire failure" {
		t.Fatalf("terminal event error=%v", event.Err)
	}
}

func TestRunTerminatedRejectsMalformedTerminalFrame(t *testing.T) {
	gateway, err := NewRelayGateway(RelayConfig{})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	for _, frame := range []runTerminated{{Status: "failed"}, {InvocationID: "invoke-1", Status: "unknown"},
		{InvocationID: "invoke-1", Status: "failed", TerminalState: "cancelled"}} {
		if err := gateway.acceptRunTerminated(frame); err == nil {
			t.Fatalf("acceptRunTerminated(%#v) succeeded", frame)
		}
	}
}
