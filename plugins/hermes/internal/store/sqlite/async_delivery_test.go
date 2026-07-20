package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func asyncRegistration(fixture runningFixture, suffix string) domain.AsyncDeliveryRegistration {
	return domain.AsyncDeliveryRegistration{
		TicketHash: "ticket-hash-" + suffix,
		Profile:    "default", ProducerEpoch: "epoch-1",
		DelegationID:    "deleg_" + suffix,
		HermesSessionID: "hermes-session-1",
		RelaySessionKey: "agent:main:relay:group:" + fixture.event.SessionID,
		ChatID:          fixture.event.SessionID + "|interactive",
		ParentRunID:     fixture.run.ID,
	}
}

func asyncCommit(ticket domain.AsyncDeliveryTicket, content string) domain.AsyncDeliveryCommit {
	return domain.AsyncDeliveryCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID,
		RelaySessionKey: ticket.RelaySessionKey, ChatID: ticket.ChatID,
		Content: content,
	}
}

func asyncOutput(t *testing.T, kind string, payload any) domain.AsyncOutput {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return domain.AsyncOutput{Kind: kind, Payload: data}
}

func TestAsyncDeliveryRegisterCopiesParentBindingAndIsIdempotent(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-register")
	registration := asyncRegistration(fixture, "register")

	first, err := value.RegisterAsyncDelivery(context.Background(), registration)
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	again, err := value.RegisterAsyncDelivery(context.Background(), registration)
	if err != nil {
		t.Fatalf("idempotent register: %v", err)
	}
	if first.ID != again.ID || first.State != domain.AsyncDeliveryPending {
		t.Fatalf("first=%#v again=%#v", first, again)
	}
	if first.Binding != fixture.event.Binding || first.ReceiverID != fixture.event.Binding.ReceiverID {
		t.Fatalf("ticket binding=%#v parent=%#v", first.Binding, fixture.event.Binding)
	}
}

func TestAsyncDeliveryCommitCreatesMultipleDurableOutboxes(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-multi")
	ticket, err := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(fixture, "multi"),
	)
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	commit := asyncCommit(ticket, "")
	commit.Outputs = []domain.AsyncOutput{
		asyncOutput(t, "text", domain.TextOutput{Content: "first"}),
		asyncOutput(t, "emoji", domain.EmojiOutput{Data: []byte("gif")}),
	}
	first, err := value.CommitAsyncDelivery(context.Background(), commit)
	if err != nil {
		t.Fatalf("CommitAsyncDelivery: %v", err)
	}
	again, err := value.CommitAsyncDelivery(context.Background(), commit)
	if err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if len(first.OutboxIDs) != 2 || first.OutboxID != first.OutboxIDs[0] {
		t.Fatalf("first=%#v", first)
	}
	if !slices.Equal(first.OutboxIDs, again.OutboxIDs) {
		t.Fatalf("first=%#v again=%#v", first, again)
	}
	text, _ := value.GetOutbox(context.Background(), first.OutboxIDs[0])
	emoji, _ := value.GetOutbox(context.Background(), first.OutboxIDs[1])
	if text.Kind != "text" || emoji.Kind != "emoji" || emoji.Sequence != text.Sequence+1 {
		t.Fatalf("text=%#v emoji=%#v", text, emoji)
	}
}

func TestAsyncDeliveryRejectsUnsupportedOutputKind(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-unsupported")
	ticket, _ := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(fixture, "unsupported"),
	)
	commit := asyncCommit(ticket, "")
	commit.Outputs = []domain.AsyncOutput{
		asyncOutput(t, "video", map[string]string{"data": "inline"}),
	}
	if _, err := value.CommitAsyncDelivery(context.Background(), commit); !errors.Is(err, storeport.ErrInvalid) {
		t.Fatalf("commit err=%v, want invalid", err)
	}
	stored, err := value.GetAsyncDelivery(context.Background(), ticket.TicketHash)
	if err != nil || stored.State != domain.AsyncDeliveryPending {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestAsyncDeliveryRejectsTicketHashReuseAcrossDelegations(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-ticket-reuse")
	first := asyncRegistration(fixture, "ticket-reuse-first")
	if _, err := value.RegisterAsyncDelivery(context.Background(), first); err != nil {
		t.Fatalf("first register: %v", err)
	}
	second := asyncRegistration(fixture, "ticket-reuse-second")
	second.TicketHash = first.TicketHash
	if _, err := value.RegisterAsyncDelivery(context.Background(), second); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("ticket reuse err=%v", err)
	}
}

func TestAsyncDeliveryRegisterRejectsFinishedParentRun(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-finished")
	if _, err := value.CommitRunSuccess(
		context.Background(), fixture.run.ID, fixture.run.LeaseToken, nil,
	); err != nil {
		t.Fatalf("CommitRunSuccess: %v", err)
	}

	_, err := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(fixture, "finished"),
	)
	if !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("register err=%v, want conflict", err)
	}
}

func TestAsyncDeliveryCommitCreatesOneDurableOutbox(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-commit")
	ticket, err := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(fixture, "commit"),
	)
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	commit := asyncCommit(ticket, "**新闻**\n- 第一条")
	first, err := value.CommitAsyncDelivery(context.Background(), commit)
	if err != nil {
		t.Fatalf("CommitAsyncDelivery: %v", err)
	}
	again, err := value.CommitAsyncDelivery(context.Background(), commit)
	if err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if first.OutboxID == "" || first.OutboxID != again.OutboxID || first.MessageID != again.MessageID {
		t.Fatalf("first=%#v again=%#v", first, again)
	}
	outbox, err := value.GetOutbox(context.Background(), first.OutboxID)
	if err != nil || outbox.RunID == fixture.run.ID || outbox.State != domain.OutboxPending {
		t.Fatalf("outbox=%#v err=%v", outbox, err)
	}
}

func TestAsyncDeliverySilentAndRevokedDoNotCreateOutbox(t *testing.T) {
	value := openStore(t)
	silentFixture := createRunningRun(t, value, "async-silent")
	silentTicket, _ := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(silentFixture, "silent"),
	)
	silentCommit := asyncCommit(silentTicket, "[silence]")
	silentCommit.Silent = true
	silent, err := value.CommitAsyncDelivery(context.Background(), silentCommit)
	if err != nil || silent.Disposition != "silent" || silent.OutboxID != "" {
		t.Fatalf("silent=%#v err=%v", silent, err)
	}

	revokedFixture := createRunningRun(t, value, "async-revoked")
	revokedRegistration := asyncRegistration(revokedFixture, "revoked")
	revokedRegistration.HermesSessionID = "hermes-session-revoked"
	revokedTicket, _ := value.RegisterAsyncDelivery(context.Background(), revokedRegistration)
	count, err := value.RevokeAsyncDeliveries(
		context.Background(), "default", "epoch-1", "hermes-session-revoked",
	)
	if err != nil || count != 1 {
		t.Fatalf("revoke count=%d err=%v", count, err)
	}
	discarded, err := value.CommitAsyncDelivery(
		context.Background(), asyncCommit(revokedTicket, "late result"),
	)
	if err != nil || discarded.Disposition != "discarded" || discarded.OutboxID != "" {
		t.Fatalf("discarded=%#v err=%v", discarded, err)
	}
}

func TestAsyncDeliveryRejectsWrongBindingAndAbandonsOldEpoch(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-binding")
	registration := asyncRegistration(fixture, "binding")
	ticket, _ := value.RegisterAsyncDelivery(context.Background(), registration)
	wrong := asyncCommit(ticket, "result")
	wrong.ChatID += "-wrong"
	if _, err := value.CommitAsyncDelivery(context.Background(), wrong); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("wrong binding err=%v", err)
	}
	count, err := value.ReconcileAsyncDeliveries(context.Background(), "default", "epoch-2")
	if err != nil || count != 1 {
		t.Fatalf("reconcile count=%d err=%v", count, err)
	}
	stored, err := value.GetAsyncDelivery(context.Background(), ticket.TicketHash)
	if err != nil || stored.State != domain.AsyncDeliveryAbandoned {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestAsyncDeliveryOutboxPreservesSessionOrder(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "async-order")
	firstTicket, _ := value.RegisterAsyncDelivery(
		context.Background(), asyncRegistration(fixture, "order-first"),
	)
	first, err := value.CommitAsyncDelivery(
		context.Background(), asyncCommit(firstTicket, "first"),
	)
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	secondRegistration := asyncRegistration(fixture, "order-second")
	secondRegistration.HermesSessionID = "hermes-session-2"
	secondTicket, _ := value.RegisterAsyncDelivery(context.Background(), secondRegistration)
	second, err := value.CommitAsyncDelivery(
		context.Background(), asyncCommit(secondTicket, "second"),
	)
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	firstOutbox, _ := value.GetOutbox(context.Background(), first.OutboxID)
	secondOutbox, _ := value.GetOutbox(context.Background(), second.OutboxID)
	if firstOutbox.SessionID != secondOutbox.SessionID ||
		secondOutbox.Sequence != firstOutbox.Sequence+1 {
		t.Fatalf("first=%#v second=%#v", firstOutbox, secondOutbox)
	}
}
