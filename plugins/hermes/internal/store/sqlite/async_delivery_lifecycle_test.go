package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
)

func TestAsyncDeliveryOutboxSurvivesReopenAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hermes.db")
	first, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	fixture := createRunningRun(t, first, "async-recover")
	ticket, err := first.RegisterAsyncDelivery(ctx, asyncRegistration(fixture, "recover"))
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	result, err := first.CommitAsyncDelivery(ctx, asyncCommit(ticket, "durable result"))
	if err != nil {
		t.Fatalf("CommitAsyncDelivery: %v", err)
	}
	leased, err := first.LeaseNextOutbox(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || leased.ID != result.OutboxID {
		t.Fatalf("LeaseNextOutbox: item=%#v err=%v", leased, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	second, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	defer second.Close()
	recovered, err := second.Recover(ctx, time.Now().Add(2*time.Second))
	if err != nil || recovered.OutboxRecovered != 1 {
		t.Fatalf("Recover: result=%#v err=%v", recovered, err)
	}
	stored, err := second.GetOutbox(ctx, result.OutboxID)
	if err != nil || stored.State != domain.OutboxRetryWait {
		t.Fatalf("GetOutbox: item=%#v err=%v", stored, err)
	}
	persisted, err := second.GetAsyncDelivery(ctx, ticket.TicketHash)
	if err != nil || persisted.State != domain.AsyncDeliveryConsumed {
		t.Fatalf("GetAsyncDelivery: ticket=%#v err=%v", persisted, err)
	}
}

func TestAsyncDeliveryCommitAndRevokeAreAtomic(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-race")
	registration := asyncRegistration(fixture, "race")
	ticket, err := value.RegisterAsyncDelivery(ctx, registration)
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}

	start := make(chan struct{})
	var committed domain.AsyncDeliveryResult
	var commitErr, revokeErr error
	var revoked int64
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		committed, commitErr = value.CommitAsyncDelivery(ctx, asyncCommit(ticket, "result"))
	}()
	go func() {
		defer wait.Done()
		<-start
		revoked, revokeErr = value.RevokeAsyncDeliveries(
			ctx, registration.Profile, registration.ProducerEpoch, registration.HermesSessionID,
		)
	}()
	close(start)
	wait.Wait()
	if commitErr != nil || revokeErr != nil {
		t.Fatalf("commit err=%v revoke err=%v", commitErr, revokeErr)
	}
	assertAsyncRaceOutcome(t, value, asyncRaceOutcome{
		ticket: ticket, committed: committed, revoked: revoked,
	})
}

type asyncRaceOutcome struct {
	ticket    domain.AsyncDeliveryTicket
	committed domain.AsyncDeliveryResult
	revoked   int64
}

func assertAsyncRaceOutcome(
	t *testing.T,
	value *sqlite.Store,
	outcome asyncRaceOutcome,
) {
	t.Helper()
	stored, err := value.GetAsyncDelivery(context.Background(), outcome.ticket.TicketHash)
	if err != nil {
		t.Fatalf("GetAsyncDelivery: %v", err)
	}
	if stored.State == domain.AsyncDeliveryConsumed {
		if outcome.revoked != 0 || outcome.committed.Disposition != "delivered" ||
			outcome.committed.DeliveryState != "queued" || outcome.committed.OutboxID == "" {
			t.Fatalf("consumed race result=%#v revoked=%d", outcome.committed, outcome.revoked)
		}
		return
	}
	if stored.State != domain.AsyncDeliveryRevoked || outcome.revoked != 1 || outcome.committed.Disposition != "discarded" {
		t.Fatalf("revoked race ticket=%#v result=%#v revoked=%d", stored, outcome.committed, outcome.revoked)
	}
	if _, err := value.GetOutbox(context.Background(), outcome.committed.OutboxID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("revoked race created outbox: %v", err)
	}
}
