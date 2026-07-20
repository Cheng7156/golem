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

func (s *Store) CommitCronDelivery(
	ctx context.Context,
	commit domain.CronDeliveryCommit,
) (domain.CronDeliveryResult, error) {
	if err := commit.Validate(); err != nil {
		return domain.CronDeliveryResult{}, err
	}
	var result domain.CronDeliveryResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		binding, err := getCronBinding(ctx, tx, commit.Profile, commit.JobID)
		if err != nil {
			return err
		}
		if binding.ChatID != commit.ChatID {
			return storeport.ErrConflict
		}
		return commitCronBinding(ctx, tx, binding, commit, &result)
	})
	return result, err
}

func commitCronBinding(
	ctx context.Context,
	tx *sql.Tx,
	binding domain.CronDeliveryBinding,
	commit domain.CronDeliveryCommit,
	result *domain.CronDeliveryResult,
) error {
	existing, err := getCronCommit(ctx, tx, binding.ID, commit.DeliveryID)
	if err == nil {
		if existing.contentHash != cronContentHash(commit.Content) {
			return storeport.ErrConflict
		}
		*result = existing.result
		return nil
	}
	if !errors.Is(err, storeport.ErrNotFound) {
		return err
	}
	return insertCronCommit(ctx, tx, binding, commit, result)
}

type storedCronCommit struct {
	contentHash string
	result      domain.CronDeliveryResult
}

func getCronCommit(
	ctx context.Context,
	tx *sql.Tx,
	bindingID string,
	deliveryID string,
) (storedCronCommit, error) {
	var value storedCronCommit
	err := tx.QueryRowContext(ctx, `
		SELECT content_hash,message_id,outbox_id FROM cron_delivery_commits
		WHERE binding_id=? AND delivery_id=?
	`, bindingID, deliveryID).Scan(
		&value.contentHash, &value.result.MessageID, &value.result.OutboxID,
	)
	if err != nil {
		return storedCronCommit{}, mapScanError(err)
	}
	value.result.Disposition = "delivered"
	return value, nil
}

func insertCronCommit(
	ctx context.Context,
	tx *sql.Tx,
	binding domain.CronDeliveryBinding,
	commit domain.CronDeliveryCommit,
	result *domain.CronDeliveryResult,
) error {
	ids := cronDeliveryIDs(binding.ID, commit.DeliveryID)
	now := time.Now()
	if err := insertCronAuditChain(ctx, tx, binding, commit, ids, now); err != nil {
		return err
	}
	outboxID, err := insertCronOutbox(ctx, tx, binding, commit.Content, ids, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO cron_delivery_commits(
			id,binding_id,delivery_id,content_hash,message_id,outbox_id,created_at
		) VALUES(?,?,?,?,?,?,?)
	`, ids.commitID, binding.ID, commit.DeliveryID, cronContentHash(commit.Content),
		ids.messageID, outboxID, unixMillis(now))
	if err != nil {
		return err
	}
	*result = domain.CronDeliveryResult{
		Disposition: "delivered", MessageID: ids.messageID, OutboxID: outboxID,
	}
	return nil
}

type cronIDs struct {
	commitID  string
	eventID   string
	turnID    string
	runID     string
	messageID string
	outboxID  string
}

func cronDeliveryIDs(bindingID string, deliveryID string) cronIDs {
	suffix := cronContentHash(bindingID + "\x00" + deliveryID)[:32]
	return cronIDs{
		commitID: "cdc_" + suffix, eventID: "event_cron_" + suffix,
		turnID: "turn_cron_" + suffix, runID: "run_cron_" + suffix,
		messageID: "cron_" + suffix, outboxID: "outbox_cron_" + suffix,
	}
}

func insertCronAuditChain(
	ctx context.Context,
	tx *sql.Tx,
	binding domain.CronDeliveryBinding,
	commit domain.CronDeliveryCommit,
	ids cronIDs,
	now time.Time,
) error {
	rawBinding, err := json.Marshal(binding.Binding)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"text": commit.Content, "speaker_id": "hermes", "speaker_name": "Hermes cron",
		"job_id": commit.JobID, "delivery_id": commit.DeliveryID,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_events(
			id,dedupe_key,message_id,topic,session_id,occurred_at,accepted_at,
			binding_json,payload_json,status,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)
	`, ids.eventID, "cron:"+binding.ID+":"+commit.DeliveryID, 0,
		"hermes.cron_delivery", binding.SessionID, unixMillis(now), unixMillis(now),
		rawBinding, payload, domain.InboxDone, unixMillis(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turns(
			id,event_id,session_id,state,route,priority,base_session_version,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)
	`, ids.turnID, ids.eventID, binding.SessionID, domain.TurnCompleted,
		domain.RouteCronDelivery, 0, 0, unixMillis(now), unixMillis(now)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs(
			id,turn_id,session_id,lane,state,revision,attempt,lease_token,lease_until,
			deadline,next_attempt_at,checkpoint_json,last_error,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, ids.runID, ids.turnID, binding.SessionID, domain.LaneJob, domain.RunSucceeded,
		0, 1, "", 0, 0, 0, nil, "", unixMillis(now), unixMillis(now))
	return err
}

func insertCronOutbox(
	ctx context.Context,
	tx *sql.Tx,
	binding domain.CronDeliveryBinding,
	content string,
	ids cronIDs,
	now time.Time,
) (string, error) {
	payload, err := json.Marshal(domain.TextOutput{Content: content})
	if err != nil {
		return "", err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence),0)+1 FROM outbox WHERE session_id=?`,
		binding.SessionID,
	).Scan(&sequence); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox(
			id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,attempt,
			lease_token,lease_until,next_attempt_at,receipt_id,receipt_time,last_error,
			created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, ids.outboxID, ids.runID, binding.SessionID, binding.ReceiverID, "text", payload,
		sequence, domain.OutboxPending, 0, "", 0, unixMillis(now), 0, 0, "",
		unixMillis(now), unixMillis(now))
	return ids.outboxID, err
}
