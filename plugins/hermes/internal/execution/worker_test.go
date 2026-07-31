package execution_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/execution"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
	"golem_plugin_hermes/internal/tool"
)

type scriptedEngine struct {
	result chan tool.Result
}

func (e *scriptedEngine) Start(_ context.Context, request agent.RunRequest) (agent.Stream, error) {
	events := make(chan agent.Event, 8)
	events <- agent.Event{Kind: agent.EventRunAccepted, RunID: request.RunID, Sequence: 1}
	events <- agent.Event{Kind: agent.EventCheckpoint, RunID: request.RunID, Sequence: 2, Checkpoint: json.RawMessage(`{"step":1}`)}
	events <- agent.Event{Kind: agent.EventToolCallRequested, RunID: request.RunID, Sequence: 3, ToolCall: &tool.Call{
		InvocationID: "invoke-1",
		RunID:        request.RunID,
		Name:         "echo",
		Arguments:    json.RawMessage(`{"value":"ok"}`),
	}}
	return &scriptedStream{runID: request.RunID, events: events, result: e.result}, nil
}

func (e *scriptedEngine) Health(context.Context) agent.Health {
	return agent.Health{Ready: true, Status: "test"}
}

func (e *scriptedEngine) Close(context.Context) error { return nil }

type scriptedStream struct {
	runID  string
	events chan agent.Event
	result chan tool.Result
	once   sync.Once
}

func (s *scriptedStream) Recv(ctx context.Context) (agent.Event, error) {
	select {
	case <-ctx.Done():
		return agent.Event{}, ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			return agent.Event{}, io.EOF
		}
		return event, nil
	}
}

func (s *scriptedStream) Send(_ context.Context, command agent.Command) error {
	if command.Kind != agent.CommandToolResult || command.ToolResult == nil {
		return errors.New("unexpected command")
	}
	s.result <- *command.ToolResult
	s.once.Do(func() {
		proposal, _ := agent.NewTextProposal("done")
		s.events <- agent.Event{Kind: agent.EventReplyProposed, RunID: s.runID, Sequence: 4, Text: "done", Proposal: &proposal}
		s.events <- agent.Event{Kind: agent.EventRunCompleted, RunID: s.runID, Sequence: 5}
		close(s.events)
	})
	return nil
}

func (s *scriptedStream) Cancel() error { return nil }
func (s *scriptedStream) Close() error  { return nil }

type echoTool struct{}

func (echoTool) Spec() tool.Spec {
	return tool.Spec{
		Name:                 "echo",
		Version:              "1",
		InputSchema:          json.RawMessage(`{"type":"object"}`),
		RequiredCapabilities: []tool.Capability{"history.read.current_session"},
		ReadOnly:             true,
		DefaultTimeout:       time.Second,
		ConcurrencyLimit:     1,
	}
}

func (echoTool) Invoke(_ context.Context, invocation tool.Invocation) (tool.Result, error) {
	return tool.Result{InvocationID: invocation.InvocationID, Output: json.RawMessage(`{"echo":"ok"}`)}, nil
}

type completingEngine struct {
	requests chan agent.RunRequest
	delay    time.Duration
	reply    string
}

func (e *completingEngine) Start(_ context.Context, request agent.RunRequest) (agent.Stream, error) {
	e.requests <- request
	events := make(chan agent.Event, 3)
	go func() {
		defer close(events)
		if e.delay > 0 {
			timer := time.NewTimer(e.delay)
			defer timer.Stop()
			<-timer.C
		}
		reply := e.reply
		if reply == "" {
			reply = "completed"
		}
		proposal, _ := agent.NewTextProposal(reply)
		events <- agent.Event{Kind: agent.EventRunAccepted, RunID: request.RunID, Sequence: 1}
		events <- agent.Event{Kind: agent.EventReplyProposed, RunID: request.RunID, Sequence: 2, Text: reply, Proposal: &proposal}
		events <- agent.Event{Kind: agent.EventRunCompleted, RunID: request.RunID, Sequence: 3}
	}()
	return agent.NewChannelStream(func() {}, events), nil
}

func (e *completingEngine) Health(context.Context) agent.Health {
	return agent.Health{Ready: true, Status: "test"}
}

func (e *completingEngine) Close(context.Context) error { return nil }

type recordingMediaResolver struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingMediaResolver) Resolve(
	_ context.Context,
	media []domain.InboundMedia,
) ([]domain.InboundMedia, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return media, nil
}

func (r *recordingMediaResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestWorkerNeverResolvesOrAttachesInboundMediaAutomatically(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	cfg := config.Default()
	cfg.Context.Mode = "legacy_shadow"
	manager, err := config.NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broker, _ := tool.NewBroker(1, 1024, nil)
	engine := &completingEngine{requests: make(chan agent.RunRequest, 2)}
	resolver := &recordingMediaResolver{}
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(
		1, store, engine, broker, resolver, domain.LaneInteractive, manager.Current,
		runWake, make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	sessionID := "chatroom:visual-history"
	owner := domain.Principal{ID: "owner", Name: "Owner", IsOwner: true}
	accept := func(id string, message domain.InboundMessage, route domain.Route) *domain.Run {
		t.Helper()
		payload, marshalErr := json.Marshal(message)
		if marshalErr != nil {
			t.Fatalf("marshal message: %v", marshalErr)
		}
		inbox, inserted, acceptErr := store.AcceptInbox(ctx, domain.InboxEvent{
			ID: id, DedupeKey: "wechat/" + id, MessageID: int64(len(id)),
			Topic: "message::image", SessionID: sessionID, OccurredAt: time.Now(),
			Binding: domain.ChannelBinding{Channel: "wechat", SessionID: sessionID,
				ReceiverID: "room", Principal: owner}, Payload: payload,
		})
		if acceptErr != nil || !inserted {
			t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, acceptErr)
		}
		turn, turnErr := store.MaterializeTurn(ctx, inbox.ID, 100)
		if turnErr != nil {
			t.Fatalf("MaterializeTurn: %v", turnErr)
		}
		lane := domain.Lane("")
		if route == domain.RouteChat {
			lane = domain.LaneInteractive
		}
		_, run, routeErr := store.RouteTurn(ctx, turn.ID, route, lane, time.Now().Add(time.Minute))
		if routeErr != nil || (route == domain.RouteChat && run == nil) {
			t.Fatalf("RouteTurn: run=%#v err=%v", run, routeErr)
		}
		return run
	}

	ordinaryRun := accept("ordinary-caption", domain.InboundMessage{
		Text: "这是我拍的午饭", IsChatroom: true, SpeakerID: "owner",
		Media: []domain.InboundMedia{{Kind: "image", MIMEType: "image/jpeg", Data: []byte("caption-image")}},
	}, domain.RouteChat)
	accept("visual-image", domain.InboundMessage{
		Text: "[image]", IsChatroom: true, SpeakerID: "owner",
		Media: []domain.InboundMedia{{Kind: "image", MIMEType: "image/jpeg", Data: []byte("stored-image")}},
	}, domain.RouteObserve)
	accept("intervening-image", domain.InboundMessage{
		Text: "[image]", IsChatroom: true, SpeakerID: "member",
		Media: []domain.InboundMedia{{Kind: "image", MIMEType: "image/jpeg", Data: []byte("member-image")}},
	}, domain.RouteObserve)
	run := accept("visual-request", domain.InboundMessage{
		Text: "@ccff 看看这张新的", IsChatroom: true, Mentioned: true, SpeakerID: "owner",
	}, domain.RouteChat)

	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}
	select {
	case request := <-engine.requests:
		if len(request.Media) != 0 {
			t.Fatalf("ordinary caption unexpectedly attached media=%#v", request.Media)
		}
		if len(request.CurrentMessage.Media) != 1 || string(request.CurrentMessage.Media[0].Data) != "caption-image" {
			t.Fatalf("current lazy media scope=%#v", request.CurrentMessage.Media)
		}
		if request.CurrentEventID != "ordinary-caption" || request.CurrentAcceptSeq <= 0 {
			t.Fatalf("current event scope=%#v", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start the ordinary caption run")
	}
	waitRunState(t, store, ordinaryRun.ID, domain.RunSucceeded)
	select {
	case request := <-engine.requests:
		if len(request.Media) != 0 {
			t.Fatalf("visual-looking text unexpectedly attached media=%#v", request.Media)
		}
		if len(request.CurrentMessage.Media) != 0 || request.CurrentEventID != "visual-request" {
			t.Fatalf("current request scope=%#v", request.CurrentMessage)
		}
		if resolver.callCount() != 0 {
			t.Fatalf("worker resolved media %d times, want 0", resolver.callCount())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start the visual follow-up run")
	}
	waitRunState(t, store, run.ID, domain.RunSucceeded)
	if resolver.callCount() != 0 {
		t.Fatalf("worker resolved media after completion %d times, want 0", resolver.callCount())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker.Run: %v", err)
	}
}

type observationCompletingEngine struct {
	*completingEngine
}

func (e *observationCompletingEngine) SupportsObservationV2() bool { return true }

func (e *observationCompletingEngine) ObserveBatch(
	context.Context,
	domain.ObservationBatch,
) (domain.ObservationAck, error) {
	return domain.ObservationAck{}, errors.New("unexpected observation dispatch from worker test")
}

type failingEngine struct {
	err error
}

func (e failingEngine) Start(context.Context, agent.RunRequest) (agent.Stream, error) {
	return nil, e.err
}

func (e failingEngine) Health(context.Context) agent.Health {
	return agent.Health{Ready: true, Status: "test"}
}

func (e failingEngine) Close(context.Context) error { return nil }

func TestWorkerPersistsCheckpointExecutesToolAndCommitsOutbox(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broker, _ := tool.NewBroker(2, 1024, nil)
	if err := broker.Register(echoTool{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	engine := &scriptedEngine{result: make(chan tool.Result, 1)}
	runWake := make(chan struct{}, 1)
	outputWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(1, store, engine, broker, nil, domain.LaneInteractive, manager.Current, runWake, outputWake)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	sessionID := "private:user-1"
	payload, _ := json.Marshal(domain.InboundMessage{Text: "hello", SpeakerID: "user-1"})
	inbox, inserted, err := store.AcceptInbox(ctx, domain.InboxEvent{
		ID: "event-1", DedupeKey: "wechat/message/1", MessageID: 1,
		Topic: "message::text", SessionID: sessionID, OccurredAt: time.Now(),
		Binding: domain.ChannelBinding{
			Channel: "wechat", SessionID: sessionID, ReceiverID: "user-1",
			Principal: domain.Principal{ID: "user-1"},
		},
		Payload: payload,
	})
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := store.MaterializeTurn(ctx, inbox.ID, 100)
	if err != nil {
		t.Fatalf("MaterializeTurn: %v", err)
	}
	_, run, err := store.RouteTurn(ctx, turn.ID, domain.RouteChat, domain.LaneInteractive, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("RouteTurn: %v", err)
	}

	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}

	select {
	case result := <-engine.result:
		if result.InvocationID != "invoke-1" || string(result.Output) != `{"echo":"ok"}` {
			t.Fatalf("tool result=%#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tool result")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		stored, err := store.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if stored.State == domain.RunSucceeded {
			if string(stored.Checkpoint) != `{"step":1}` {
				t.Fatalf("checkpoint=%s", stored.Checkpoint)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not succeed: %#v", stored)
		}
		time.Sleep(10 * time.Millisecond)
	}
	item, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("LeaseNextOutbox: %v", err)
	}
	if item.Kind != "text" || string(item.Payload) != `{"content":"done"}` {
		t.Fatalf("outbox=%#v", item)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Worker.Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestRelayWorkerIgnoresLegacyAbsoluteDeadline(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broker, _ := tool.NewBroker(1, 1024, nil)
	engine := &completingEngine{requests: make(chan agent.RunRequest, 1), delay: 250 * time.Millisecond}
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(
		1, store, engine, broker, nil, domain.LaneInteractive,
		manager.Current, runWake, make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	// Simulate a message that spent longer than the legacy 120-second budget
	// waiting behind another turn before a worker could lease it.
	run := createRun(t, store, time.Now().Add(-time.Minute))

	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}

	select {
	case request := <-engine.requests:
		if !request.Deadline.IsZero() {
			t.Fatalf("Relay request inherited connector deadline: %v", request.Deadline)
		}
		if request.PlatformMessageID != "1" {
			t.Fatalf("platform message id=%q, want 1", request.PlatformMessageID)
		}
		if !request.RequireVisibleReply {
			t.Fatal("private Relay request did not require a visible reply")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Relay engine was not started")
	}
	waitRunState(t, store, run.ID, domain.RunSucceeded)
	item, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("LeaseNextOutbox: %v", err)
	}
	if string(item.Payload) != `{"content":"completed"}` {
		t.Fatalf("outbox payload=%s", item.Payload)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker.Run: %v", err)
	}
}

func TestWorkerPreservesModelReplyBeforeOutboxCommit(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broker, _ := tool.NewBroker(1, 1024, nil)
	engine := &completingEngine{
		requests: make(chan agent.RunRequest, 1),
		reply:    "嗨呀主人～ 有啥事儿吗？",
	}
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(
		1, store, engine, broker, nil, domain.LaneInteractive,
		manager.Current, runWake, make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	run := createRun(t, store, time.Now().Add(time.Minute))

	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}
	select {
	case <-engine.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("Relay engine was not started")
	}
	waitRunState(t, store, run.ID, domain.RunSucceeded)
	item, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("LeaseNextOutbox: %v", err)
	}
	var output domain.TextOutput
	if err := json.Unmarshal(item.Payload, &output); err != nil {
		t.Fatal(err)
	}
	if output.Content != engine.reply {
		t.Fatalf("outbox content=%q", output.Content)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker.Run: %v", err)
	}
}

func TestWorkerBoundsObservationBarrierAndCarriesCurrentFallback(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Default()
	cfg.Context.Mode = "full"
	cfg.Context.BarrierWaitMilliseconds = 40
	manager, err := config.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	baseEngine := &completingEngine{requests: make(chan agent.RunRequest, 1)}
	engine := &observationCompletingEngine{completingEngine: baseEngine}
	broker, _ := tool.NewBroker(1, 1024, nil)
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(1, store, engine, broker, nil, domain.LaneInteractive,
		manager.Current, runWake, make(chan struct{}, 1))
	if err != nil {
		t.Fatal(err)
	}
	run := createRun(t, store, time.Now().Add(time.Minute))
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}

	select {
	case request := <-baseEngine.requests:
		if !request.ContextLagFallback || request.CurrentObservation == nil {
			t.Fatalf("request did not use lag fallback: %#v", request)
		}
		if request.CurrentObservation.ObservationID != run.CurrentObservationID ||
			request.CurrentObservation.PayloadHash != run.CurrentPayloadHash {
			t.Fatalf("fallback observation=%#v run=%#v", request.CurrentObservation, run)
		}
	case <-time.After(time.Second):
		t.Fatal("worker remained blocked behind observation barrier")
	}
	waitRunState(t, store, run.ID, domain.RunSucceeded)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker.Run: %v", err)
	}
}

func TestTerminalRunFailureDoesNotCreateChatFallback(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	value := config.Default()
	value.Agent.Mode = "http"
	manager, err := config.NewManager(value)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broker, _ := tool.NewBroker(1, 1024, nil)
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(
		1, store, failingEngine{err: errors.New("permanent adapter failure")},
		broker, nil, domain.LaneInteractive, manager.Current, runWake,
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	run := createRun(t, store, time.Now().Add(-time.Minute))

	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}
	waitRunState(t, store, run.ID, domain.RunFailed)
	if _, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("terminal failure created a chat outbox item: %v", err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker.Run: %v", err)
	}
}

func TestRelayDisconnectRetainsRunWithoutChatFallback(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	manager, err := config.NewManager(config.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broker, _ := tool.NewBroker(1, 1024, nil)
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(
		1, store, failingEngine{err: agent.ErrGatewayDisconnected},
		broker, nil, domain.LaneInteractive, manager.Current, runWake,
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	run := createRun(t, store, time.Now().Add(-time.Minute))

	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	runWake <- struct{}{}
	stored := waitRunState(t, store, run.ID, domain.RunRetryWait)
	if stored.LastError != agent.ErrGatewayDisconnected.Error() {
		t.Fatalf("last_error=%q", stored.LastError)
	}
	turn, err := store.GetTurn(ctx, run.TurnID)
	if err != nil {
		t.Fatalf("GetTurn: %v", err)
	}
	if turn.State != domain.TurnRunning {
		t.Fatalf("turn state=%s, want recoverable running", turn.State)
	}
	if _, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("Relay disconnect created a chat outbox item: %v", err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker.Run: %v", err)
	}
}

func createRun(t *testing.T, store *sqlite.Store, deadline time.Time) domain.Run {
	t.Helper()
	ctx := context.Background()
	sessionID := "private:user-1"
	payload, _ := json.Marshal(domain.InboundMessage{Text: "hello", SpeakerID: "user-1"})
	inbox, inserted, err := store.AcceptInbox(ctx, domain.InboxEvent{
		ID: "event-" + t.Name(), DedupeKey: "wechat/" + t.Name(), MessageID: 1,
		Topic: "message::text", SessionID: sessionID, OccurredAt: time.Now(),
		Binding: domain.ChannelBinding{
			Channel: "wechat", SessionID: sessionID, ReceiverID: "user-1",
			Principal: domain.Principal{ID: "user-1"},
		},
		Payload: payload,
	})
	if err != nil || !inserted {
		t.Fatalf("AcceptInbox: inserted=%v err=%v", inserted, err)
	}
	turn, err := store.MaterializeTurn(ctx, inbox.ID, 100)
	if err != nil {
		t.Fatalf("MaterializeTurn: %v", err)
	}
	_, run, err := store.RouteTurn(ctx, turn.ID, domain.RouteChat, domain.LaneInteractive, deadline)
	if err != nil || run == nil {
		t.Fatalf("RouteTurn: run=%#v err=%v", run, err)
	}
	return *run
}

func waitRunState(t *testing.T, store *sqlite.Store, runID string, want domain.RunState) domain.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		run, err := store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.State == want {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run state=%s, want %s; last_error=%q", run.State, want, run.LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
