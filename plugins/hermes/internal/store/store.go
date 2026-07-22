package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
)

var (
	ErrNotFound = errors.New("hermes store: not found")
	ErrConflict = errors.New("hermes store: state conflict")
	ErrClosed   = errors.New("hermes store: closed")
	ErrInvalid  = errors.New("hermes store: invalid input")
)

type Store interface {
	Close() error

	AcceptInbox(context.Context, domain.InboxEvent) (domain.InboxEvent, bool, error)
	GetInbox(context.Context, string) (domain.InboxEvent, error)
	ListInbox(context.Context, domain.InboxStatus, int) ([]domain.InboxEvent, error)
	TransitionInbox(context.Context, string, domain.InboxStatus, domain.InboxStatus) error
	LeaseNextObservationBatch(context.Context, time.Time, time.Duration, int) (domain.ObservationBatch, error)
	MarkObservationBatchAcked(context.Context, domain.ObservationBatch, domain.ObservationAck) error
	MarkObservationBatchRetry(context.Context, domain.ObservationBatch, string, time.Time) error
	MarkObservationBatchTerminal(context.Context, domain.ObservationBatch, domain.ContextOutboxState, string) error
	ObservationContextReady(context.Context, string, int64) (bool, error)
	GetContextOutboxByEvent(context.Context, string) (domain.ContextOutboxItem, error)

	CreateTurn(context.Context, domain.Turn) (domain.Turn, error)
	MaterializeTurn(context.Context, string, int) (domain.Turn, error)
	GetTurn(context.Context, string) (domain.Turn, error)
	ListTurns(context.Context, domain.TurnState, int) ([]domain.Turn, error)
	RouteTurn(context.Context, string, domain.Route, domain.Lane, time.Time) (domain.Turn, *domain.Run, error)
	TransitionTurn(context.Context, string, domain.TurnState, domain.Route) error

	CreateRun(context.Context, domain.Run) (domain.Run, error)
	GetRun(context.Context, string) (domain.Run, error)
	LeaseNextRun(context.Context, domain.Lane, time.Time, time.Duration) (domain.Run, error)
	LeaseNextRunByTrigger(context.Context, domain.Lane, domain.TriggerKind, time.Time, time.Duration) (domain.Run, error)
	ReconcileRunAdmission(context.Context, string, domain.RunAdmissionMode) (domain.RunAdmissionResult, error)
	MarkRunRunning(context.Context, string, string) error
	SaveRunCheckpoint(context.Context, string, string, json.RawMessage) error
	RequestRunCancel(context.Context, string) error
	RequestSessionCancel(context.Context, string, string) ([]string, error)
	MarkRunCancelled(context.Context, string, string) error
	FailRun(context.Context, string, string, string, bool, time.Time) error
	CommitRunSuccess(context.Context, string, string, []domain.OutboxDraft) ([]domain.OutboxItem, error)
	CommitRunFailure(context.Context, string, string, string, []domain.OutboxDraft) ([]domain.OutboxItem, error)
	CommitRelayRunResult(context.Context, string, string, domain.RelayRunResult, []domain.OutboxDraft) ([]domain.OutboxItem, error)
	GetRelayRunResult(context.Context, string) (domain.RelayRunResult, error)
	RegisterAsyncDelivery(context.Context, domain.AsyncDeliveryRegistration) (domain.AsyncDeliveryTicket, error)
	GetAsyncDelivery(context.Context, string) (domain.AsyncDeliveryTicket, error)
	CommitAsyncDelivery(context.Context, domain.AsyncDeliveryCommit) (domain.AsyncDeliveryResult, error)
	CommitAsyncDirectOutput(context.Context, domain.AsyncDirectOutputCommit) (domain.AsyncDirectOutputResult, error)
	CountAsyncDirectOutputs(context.Context, string) (int64, error)
	RevokeAsyncDeliveries(context.Context, string, string, string) (int64, error)
	ReconcileAsyncDeliveries(context.Context, string, string) (int64, error)
	RegisterCronDelivery(
		context.Context,
		domain.CronDeliveryRegistration,
	) (domain.CronDeliveryBinding, error)
	CommitCronDelivery(
		context.Context,
		domain.CronDeliveryCommit,
	) (domain.CronDeliveryResult, error)
	GetCronDelivery(context.Context, string, string) (domain.CronDeliveryBinding, error)
	CommitCronDirectOutput(
		context.Context,
		domain.CronDirectOutputCommit,
	) (domain.AsyncDirectOutputResult, error)
	CountCronDirectOutputs(context.Context, domain.CronDirectOutputScope) (int64, error)

	LeaseNextOutbox(context.Context, time.Time, time.Duration) (domain.OutboxItem, error)
	MarkOutboxSent(context.Context, string, string, uint64, time.Time) error
	MarkOutboxRetry(context.Context, string, string, string, string, time.Time) error
	MarkOutboxDeadLetter(context.Context, string, string, string, string) error
	GetOutbox(context.Context, string) (domain.OutboxItem, error)
	ListDeliveryAttempts(context.Context, string) ([]domain.DeliveryAttempt, error)
	CreateMediaObject(context.Context, domain.MediaObject) (domain.MediaObject, bool, error)
	GetMediaObject(context.Context, string) (domain.MediaObject, error)
	RetainMediaObject(context.Context, string, time.Time) error
	MediaStorageBytes(context.Context) (int64, error)
	ListCollectibleMediaObjects(context.Context, time.Time, int) ([]domain.MediaObject, error)
	DeleteMediaObject(context.Context, string) error

	Recover(context.Context, time.Time) (domain.RecoveryResult, error)
}
