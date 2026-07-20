package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/output"
	storeport "golem_plugin_hermes/internal/store"
)

const (
	concurrentDirectOutputs = 5
	dispatchedDirectOutputs = 3
)

type recordingDirectSender struct {
	mu  sync.Mutex
	ids []string
}

func (s *recordingDirectSender) Send(
	_ context.Context,
	item domain.OutboxItem,
) (output.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, item.ID)
	return output.Receipt{ID: uint64(len(s.ids))}, nil
}

func (s *recordingDirectSender) sentIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

func directEmojiCommit(
	t *testing.T,
	ticket domain.AsyncDeliveryTicket,
	invocationID string,
) domain.AsyncDirectOutputCommit {
	t.Helper()
	return domain.AsyncDirectOutputCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID,
		RelaySessionKey: ticket.RelaySessionKey, ChatID: ticket.ChatID,
		InvocationID: invocationID,
		Output:       asyncOutput(t, "emoji", domain.EmojiOutput{Data: []byte("gif")}),
	}
}

func TestAsyncDirectOutputIsIdempotentAndOrdered(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-direct")
	ticket, err := value.RegisterAsyncDelivery(ctx, asyncRegistration(fixture, "direct"))
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	firstCommit := directEmojiCommit(t, ticket, "call-1")
	first, err := value.CommitAsyncDirectOutput(ctx, firstCommit)
	if err != nil {
		t.Fatalf("CommitAsyncDirectOutput: %v", err)
	}
	again, err := value.CommitAsyncDirectOutput(ctx, firstCommit)
	if err != nil {
		t.Fatalf("idempotent direct output: %v", err)
	}
	second, err := value.CommitAsyncDirectOutput(
		ctx, directEmojiCommit(t, ticket, "call-2"),
	)
	if err != nil {
		t.Fatalf("second direct output: %v", err)
	}
	if first.OutboxID != again.OutboxID || first.DirectOutputCount != 1 {
		t.Fatalf("first=%#v again=%#v", first, again)
	}
	changed := firstCommit
	changed.Output = asyncOutput(
		t, "emoji", domain.EmojiOutput{Data: []byte("different")},
	)
	if _, err := value.CommitAsyncDirectOutput(ctx, changed); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("reused invocation with changed output err=%v", err)
	}
	if second.DirectOutputCount != 2 || second.Sequence != first.Sequence+1 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	count, err := value.CountAsyncDirectOutputs(ctx, ticket.TicketHash)
	if err != nil || count != 2 {
		t.Fatalf("direct count=%d err=%v", count, err)
	}
	completion := asyncCommit(ticket, "")
	completion.Silent = true
	closed, err := value.CommitAsyncDelivery(ctx, completion)
	if err != nil || closed.Disposition != "silent" {
		t.Fatalf("silent completion=%#v err=%v", closed, err)
	}
	late := directEmojiCommit(t, ticket, "call-late")
	if _, err := value.CommitAsyncDirectOutput(ctx, late); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("late direct output err=%v", err)
	}
}

func TestAsyncDirectOutputRejectsWrongOrInactiveBinding(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-direct-binding")
	registration := asyncRegistration(fixture, "direct-binding")
	registration.HermesSessionID = "direct-session"
	ticket, err := value.RegisterAsyncDelivery(ctx, registration)
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	wrong := directEmojiCommit(t, ticket, "call-wrong")
	wrong.ChatID += "-wrong"
	if _, err := value.CommitAsyncDirectOutput(ctx, wrong); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("wrong binding err=%v", err)
	}
	if _, err := value.RevokeAsyncDeliveries(
		ctx, ticket.Profile, ticket.ProducerEpoch, ticket.HermesSessionID,
	); err != nil {
		t.Fatalf("RevokeAsyncDeliveries: %v", err)
	}
	valid := directEmojiCommit(t, ticket, "call-revoked")
	if _, err := value.CommitAsyncDirectOutput(ctx, valid); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("revoked ticket err=%v", err)
	}
}

func TestAsyncDirectOutputRequiresInvocationID(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-direct-invocation")
	ticket, _ := value.RegisterAsyncDelivery(
		ctx, asyncRegistration(fixture, "direct-invocation"),
	)
	commit := directEmojiCommit(t, ticket, "")
	if _, err := value.CommitAsyncDirectOutput(ctx, commit); !errors.Is(err, storeport.ErrInvalid) {
		t.Fatalf("missing invocation err=%v", err)
	}
}

func TestConcurrentAsyncDirectOutputsGetUniqueSessionOrder(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-direct-concurrent")
	ticket, _ := value.RegisterAsyncDelivery(
		ctx, asyncRegistration(fixture, "direct-concurrent"),
	)
	results := make(chan domain.AsyncDirectOutputResult, concurrentDirectOutputs)
	errorsOut := make(chan error, concurrentDirectOutputs)
	var group sync.WaitGroup
	for index := range concurrentDirectOutputs {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := value.CommitAsyncDirectOutput(
				ctx, directEmojiCommit(t, ticket, fmt.Sprintf("call-%d", index)),
			)
			results <- result
			errorsOut <- err
		}()
	}
	group.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("concurrent direct output: %v", err)
		}
	}
	sequences := make([]int, 0, concurrentDirectOutputs)
	for result := range results {
		sequences = append(sequences, int(result.Sequence))
	}
	sort.Ints(sequences)
	for index := 1; index < len(sequences); index++ {
		if sequences[index] != sequences[index-1]+1 {
			t.Fatalf("non-contiguous sequences=%v", sequences)
		}
	}
	count, _ := value.CountAsyncDirectOutputs(ctx, ticket.TicketHash)
	if count != concurrentDirectOutputs {
		t.Fatalf("direct count=%d", count)
	}
}

func TestDispatcherSendsEveryQueuedDirectOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-direct-dispatch")
	ticket, _ := value.RegisterAsyncDelivery(
		ctx, asyncRegistration(fixture, "direct-dispatch"),
	)
	outboxIDs := commitDirectOutputs(t, value, ticket)
	cfg := config.Default()
	cfg.Output.SendIntervalMilliseconds = 0
	manager, err := config.NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sender := new(recordingDirectSender)
	dispatcher, err := output.NewDispatcher(value, sender, manager.Current, make(chan struct{}, 1))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	waitDirectOutputsSent(t, value, outboxIDs)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatcher.Run: %v", err)
	}
	if sent := sender.sentIDs(); len(sent) != len(outboxIDs) {
		t.Fatalf("sent IDs=%v, want %v", sent, outboxIDs)
	}
}

func commitDirectOutputs(
	t *testing.T,
	value directOutputStore,
	ticket domain.AsyncDeliveryTicket,
) []string {
	t.Helper()
	result := make([]string, 0, dispatchedDirectOutputs)
	for index := range dispatchedDirectOutputs {
		queued, err := value.CommitAsyncDirectOutput(
			context.Background(),
			directEmojiCommit(t, ticket, fmt.Sprintf("dispatch-%d", index)),
		)
		if err != nil || !queued.Queued {
			t.Fatalf("commit %d: result=%#v err=%v", index, queued, err)
		}
		result = append(result, queued.OutboxID)
	}
	return result
}

type directOutputStore interface {
	CommitAsyncDirectOutput(
		context.Context,
		domain.AsyncDirectOutputCommit,
	) (domain.AsyncDirectOutputResult, error)
	GetOutbox(context.Context, string) (domain.OutboxItem, error)
}

func waitDirectOutputsSent(t *testing.T, value directOutputStore, outboxIDs []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allSent := true
		for _, outboxID := range outboxIDs {
			item, err := value.GetOutbox(context.Background(), outboxID)
			if err != nil || item.State != domain.OutboxSent {
				allSent = false
				break
			}
		}
		if allSent {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("direct outputs did not all reach sent: %v", outboxIDs)
}
