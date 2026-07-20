package execution_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/execution"
	"golem_plugin_hermes/internal/store/sqlite"
	"golem_plugin_hermes/internal/tool"
)

type textAndStickerEngine struct{}

func (textAndStickerEngine) Start(_ context.Context, request agent.RunRequest) (agent.Stream, error) {
	events := make(chan agent.Event, 4)
	text, _ := agent.NewTextProposal("文字回复")
	emojiPayload, _ := json.Marshal(domain.EmojiOutput{Data: []byte("GIF89a"), Description: "开心"})
	emoji := agent.OutputProposal{Kind: "emoji", Payload: emojiPayload}
	events <- agent.Event{Kind: agent.EventRunAccepted, RunID: request.RunID, Sequence: 1}
	events <- agent.Event{Kind: agent.EventReplyProposed, RunID: request.RunID, Sequence: 2, Proposal: &text}
	events <- agent.Event{Kind: agent.EventEffectProposed, RunID: request.RunID, Sequence: 3, Proposal: &emoji}
	events <- agent.Event{Kind: agent.EventRunCompleted, RunID: request.RunID, Sequence: 4}
	close(events)
	return agent.NewChannelStream(func() {}, events), nil
}

func (textAndStickerEngine) Health(context.Context) agent.Health {
	return agent.Health{Ready: true, Status: "test"}
}

func (textAndStickerEngine) Close(context.Context) error { return nil }

func TestWorkerCommitsTextAndStickerToOrderedOutbox(t *testing.T) {
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
	broker, err := tool.NewBroker(1, 1024, nil)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	runWake := make(chan struct{}, 1)
	worker, err := execution.NewWorker(
		1, store, textAndStickerEngine{}, broker, nil, domain.LaneInteractive,
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
	waitRunState(t, store, run.ID, domain.RunSucceeded)

	first, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("Lease text: %v", err)
	}
	if first.RunID != run.ID || first.Kind != "text" || first.Sequence != 1 {
		t.Fatalf("first outbox=%#v", first)
	}
	if err := store.MarkOutboxSent(ctx, first.ID, first.LeaseToken, 1, time.Now()); err != nil {
		t.Fatalf("Mark text sent: %v", err)
	}
	second, err := store.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("Lease emoji: %v", err)
	}
	if second.RunID != run.ID || second.Kind != "emoji" || second.Sequence != 2 {
		t.Fatalf("second outbox=%#v", second)
	}
	var emoji domain.EmojiOutput
	if err := json.Unmarshal(second.Payload, &emoji); err != nil || string(emoji.Data) != "GIF89a" {
		t.Fatalf("emoji=%#v err=%v", emoji, err)
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
