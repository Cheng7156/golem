package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type scanner interface {
	Scan(...any) error
}

const inboxColumns = `
id,dedupe_key,message_id,topic,session_id,occurred_at,accepted_at,
binding_json,payload_json,status,accept_seq
`

func scanInbox(row scanner) (domain.InboxEvent, error) {
	var event domain.InboxEvent
	var occurredAt, acceptedAt int64
	var binding []byte
	if err := row.Scan(
		&event.ID,
		&event.DedupeKey,
		&event.MessageID,
		&event.Topic,
		&event.SessionID,
		&occurredAt,
		&acceptedAt,
		&binding,
		&event.Payload,
		&event.Status,
		&event.AcceptSeq,
	); err != nil {
		return domain.InboxEvent{}, mapScanError(err)
	}
	if err := json.Unmarshal(binding, &event.Binding); err != nil {
		return domain.InboxEvent{}, fmt.Errorf("解析 ChannelBinding: %w", err)
	}
	event.OccurredAt = fromUnixMillis(occurredAt)
	event.AcceptedAt = fromUnixMillis(acceptedAt)
	event.Payload = cloneBytes(event.Payload)
	return event, nil
}

const turnColumns = `
id,event_id,session_id,state,route,priority,base_session_version,created_at,updated_at
`

func scanTurn(row scanner) (domain.Turn, error) {
	var value domain.Turn
	var createdAt, updatedAt int64
	if err := row.Scan(
		&value.ID,
		&value.EventID,
		&value.SessionID,
		&value.State,
		&value.Route,
		&value.Priority,
		&value.BaseSessionVersion,
		&createdAt,
		&updatedAt,
	); err != nil {
		return domain.Turn{}, mapScanError(err)
	}
	value.CreatedAt = fromUnixMillis(createdAt)
	value.UpdatedAt = fromUnixMillis(updatedAt)
	return value, nil
}

const runColumns = `
r.id,r.turn_id,r.session_id,r.lane,r.state,r.revision,r.attempt,
r.lease_token,r.lease_until,r.deadline,r.next_attempt_at,r.checkpoint_json,
r.last_error,r.created_at,r.updated_at
`

const directRunColumns = `
id,turn_id,session_id,lane,state,revision,attempt,
lease_token,lease_until,deadline,next_attempt_at,checkpoint_json,
last_error,created_at,updated_at
`

func scanRun(row scanner) (domain.Run, error) {
	var value domain.Run
	var leaseUntil, deadline, nextAttempt, createdAt, updatedAt int64
	var checkpoint []byte
	if err := row.Scan(
		&value.ID,
		&value.TurnID,
		&value.SessionID,
		&value.Lane,
		&value.State,
		&value.Revision,
		&value.Attempt,
		&value.LeaseToken,
		&leaseUntil,
		&deadline,
		&nextAttempt,
		&checkpoint,
		&value.LastError,
		&createdAt,
		&updatedAt,
	); err != nil {
		return domain.Run{}, mapScanError(err)
	}
	value.LeaseUntil = fromUnixMillis(leaseUntil)
	value.Deadline = fromUnixMillis(deadline)
	value.NextAttempt = fromUnixMillis(nextAttempt)
	value.CreatedAt = fromUnixMillis(createdAt)
	value.UpdatedAt = fromUnixMillis(updatedAt)
	value.Checkpoint = cloneBytes(checkpoint)
	return value, nil
}

const outboxColumns = `
id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,attempt,
lease_token,lease_until,next_attempt_at,receipt_id,receipt_time,last_error,
created_at,updated_at
`

func scanOutbox(row scanner) (domain.OutboxItem, error) {
	var value domain.OutboxItem
	var leaseUntil, nextAttempt, receiptTime, createdAt, updatedAt int64
	if err := row.Scan(
		&value.ID,
		&value.RunID,
		&value.SessionID,
		&value.ReceiverID,
		&value.Kind,
		&value.Payload,
		&value.Sequence,
		&value.State,
		&value.Attempt,
		&value.LeaseToken,
		&leaseUntil,
		&nextAttempt,
		&value.ReceiptID,
		&receiptTime,
		&value.LastError,
		&createdAt,
		&updatedAt,
	); err != nil {
		return domain.OutboxItem{}, mapScanError(err)
	}
	value.LeaseUntil = fromUnixMillis(leaseUntil)
	value.NextAttempt = fromUnixMillis(nextAttempt)
	value.ReceiptTime = fromUnixMillis(receiptTime)
	value.CreatedAt = fromUnixMillis(createdAt)
	value.UpdatedAt = fromUnixMillis(updatedAt)
	value.Payload = cloneBytes(value.Payload)
	return value, nil
}

func mapScanError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return storeport.ErrNotFound
	}
	return err
}
