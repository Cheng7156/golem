package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type cronDirectInsert struct {
	binding  domain.CronDeliveryBinding
	commit   domain.CronDirectOutputCommit
	ids      cronDirectIDs
	now      time.Time
	sequence int64
}

type cronDirectScope struct {
	bindingID  string
	deliveryID string
}

type cronDirectAudit struct {
	insert     cronDirectInsert
	rawBinding []byte
	payload    []byte
}

func (s *Store) CommitCronDirectOutput(
	ctx context.Context,
	commit domain.CronDirectOutputCommit,
) (domain.AsyncDirectOutputResult, error) {
	if err := commit.Validate(); err != nil {
		return domain.AsyncDirectOutputResult{}, errors.Join(storeport.ErrInvalid, err)
	}
	var result domain.AsyncDirectOutputResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		binding, err := getCronBinding(ctx, tx, commit.Profile, commit.JobID)
		if err != nil {
			return err
		}
		input := cronDirectInsert{binding: binding, commit: commit}
		existing, ok, err := existingCronDirectOutput(ctx, tx, input)
		if err != nil {
			return err
		}
		if ok {
			result = existing
			return nil
		}
		result, err = insertCronDirectOutput(ctx, tx, input)
		return err
	})
	return result, err
}

func (s *Store) CountCronDirectOutputs(
	ctx context.Context,
	scope domain.CronDirectOutputScope,
) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, errors.Join(storeport.ErrInvalid, err)
	}
	db, err := s.readable()
	if err != nil {
		return 0, err
	}
	var count int64
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(d.id) FROM cron_delivery_bindings b
		LEFT JOIN cron_delivery_direct_outputs d
		  ON d.binding_id=b.id AND d.delivery_id=?
		WHERE b.profile=? AND b.job_id=? GROUP BY b.id
	`, scope.DeliveryID, scope.Profile, scope.JobID).Scan(&count)
	return count, mapScanError(err)
}

func existingCronDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	input cronDirectInsert,
) (domain.AsyncDirectOutputResult, bool, error) {
	var result domain.AsyncDirectOutputResult
	var contentHash string
	err := tx.QueryRowContext(ctx, `
		SELECT d.content_hash,d.outbox_id,o.sequence
		FROM cron_delivery_direct_outputs d
		JOIN outbox o ON o.id=d.outbox_id
		WHERE d.binding_id=? AND d.delivery_id=? AND d.invocation_id=?
	`, input.binding.ID, input.commit.DeliveryID, input.commit.InvocationID).Scan(
		&contentHash, &result.OutboxID, &result.Sequence,
	)
	mapped := mapScanError(err)
	if errors.Is(mapped, storeport.ErrNotFound) {
		return result, false, nil
	}
	if mapped != nil {
		return result, false, mapped
	}
	if contentHash != directOutputHash(input.commit.Output) {
		return result, false, storeport.ErrConflict
	}
	count, err := countCronDirectOutputs(ctx, tx, input.scope())
	result.Queued, result.DirectOutputCount = true, count
	return result, true, err
}

func insertCronDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	input cronDirectInsert,
) (domain.AsyncDirectOutputResult, error) {
	if err := validateAsyncOutputPayload(input.commit.Output); err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	prepared := cronDirectInsert{
		binding: input.binding, commit: input.commit,
		ids: newCronDirectIDs(
			input.binding.ID, input.commit.DeliveryID, input.commit.InvocationID,
		),
		now: time.Now(),
	}
	if err := insertCronDirectAudit(ctx, tx, prepared); err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	sequence, err := insertCronDirectOutbox(ctx, tx, prepared)
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	recorded := cronDirectInsert{
		binding: prepared.binding, commit: prepared.commit,
		ids: prepared.ids, now: prepared.now, sequence: sequence,
	}
	return recordCronDirectOutput(ctx, tx, recorded)
}

func (i cronDirectInsert) scope() cronDirectScope {
	return cronDirectScope{bindingID: i.binding.ID, deliveryID: i.commit.DeliveryID}
}

type cronDirectIDs struct {
	eventID string
	turnID  string
	runID   string
	outbox  string
	record  string
}

func newCronDirectIDs(bindingID string, deliveryID string, invocationID string) cronDirectIDs {
	suffix := cronContentHash(bindingID + "\x00" + deliveryID + "\x00" + invocationID)[:32]
	return cronDirectIDs{
		eventID: "event_cron_direct_" + suffix,
		turnID:  "turn_cron_direct_" + suffix,
		runID:   "run_cron_direct_" + suffix,
		outbox:  "outbox_cron_direct_" + suffix,
		record:  "cdo_" + suffix,
	}
}

func insertCronDirectAudit(
	ctx context.Context,
	tx *sql.Tx,
	input cronDirectInsert,
) error {
	rawBinding, err := json.Marshal(input.binding.Binding)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"outputs": []domain.AsyncOutput{input.commit.Output}, "speaker_id": "hermes",
		"speaker_name": "Hermes cron", "job_id": input.commit.JobID,
		"delivery_id": input.commit.DeliveryID, "invocation_id": input.commit.InvocationID,
	})
	if err != nil {
		return err
	}
	audit := cronDirectAudit{insert: input, rawBinding: rawBinding, payload: payload}
	if err := insertCronDirectEvent(ctx, tx, audit); err != nil {
		return err
	}
	return insertCronDirectRun(ctx, tx, input)
}

func insertCronDirectEvent(
	ctx context.Context,
	tx *sql.Tx,
	audit cronDirectAudit,
) error {
	input := audit.insert
	_, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_events(
			id,dedupe_key,message_id,topic,session_id,occurred_at,accepted_at,
			binding_json,payload_json,status,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)
	`, input.ids.eventID, "cron-direct:"+input.binding.ID+":"+input.commit.DeliveryID+":"+input.commit.InvocationID,
		0, "hermes.cron_direct_delivery", input.binding.SessionID, unixMillis(input.now),
		unixMillis(input.now), audit.rawBinding, audit.payload,
		domain.InboxDone, unixMillis(input.now))
	return err
}

func insertCronDirectRun(
	ctx context.Context,
	tx *sql.Tx,
	input cronDirectInsert,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turns(
			id,event_id,session_id,state,route,priority,base_session_version,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)
	`, input.ids.turnID, input.ids.eventID, input.binding.SessionID, domain.TurnCompleted,
		domain.RouteCronDelivery, 0, 0, unixMillis(input.now), unixMillis(input.now)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO runs(
			id,turn_id,session_id,lane,state,revision,attempt,lease_token,lease_until,
			deadline,next_attempt_at,checkpoint_json,last_error,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, input.ids.runID, input.ids.turnID, input.binding.SessionID, domain.LaneJob, domain.RunSucceeded,
		0, 1, "", 0, 0, 0, nil, "", unixMillis(input.now), unixMillis(input.now))
	return err
}

func insertCronDirectOutbox(
	ctx context.Context,
	tx *sql.Tx,
	input cronDirectInsert,
) (int64, error) {
	sequence, err := nextOutboxSequence(ctx, tx, input.binding.SessionID)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox(
			id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,attempt,
			lease_token,lease_until,next_attempt_at,receipt_id,receipt_time,last_error,
			created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, input.ids.outbox, input.ids.runID, input.binding.SessionID,
		input.binding.ReceiverID, input.commit.Output.Kind,
		[]byte(input.commit.Output.Payload), sequence, domain.OutboxPending, 0, "", 0,
		unixMillis(input.now), 0, 0, "", unixMillis(input.now), unixMillis(input.now))
	return sequence, err
}

func recordCronDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	input cronDirectInsert,
) (domain.AsyncDirectOutputResult, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO cron_delivery_direct_outputs(
			id,binding_id,delivery_id,invocation_id,content_hash,outbox_id,created_at
		) VALUES(?,?,?,?,?,?,?)
	`, input.ids.record, input.binding.ID, input.commit.DeliveryID,
		input.commit.InvocationID, directOutputHash(input.commit.Output),
		input.ids.outbox, unixMillis(input.now))
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	count, err := countCronDirectOutputs(ctx, tx, input.scope())
	return domain.AsyncDirectOutputResult{
		Queued: true, OutboxID: input.ids.outbox,
		Sequence: input.sequence, DirectOutputCount: count,
	}, err
}

func countCronDirectOutputs(
	ctx context.Context,
	tx *sql.Tx,
	scope cronDirectScope,
) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM cron_delivery_direct_outputs
		WHERE binding_id=? AND delivery_id=?
	`, scope.bindingID, scope.deliveryID).Scan(&count)
	return count, err
}
