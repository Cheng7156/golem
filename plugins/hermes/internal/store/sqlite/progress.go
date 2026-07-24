package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const maxRunProgressIDLength = 256

func (s *Store) CommitRunProgress(
	ctx context.Context,
	runID string,
	leaseToken string,
	progressID string,
	draft domain.OutboxDraft,
	maxMessages int,
) (domain.OutboxItem, error) {
	runID = strings.TrimSpace(runID)
	progressID = strings.TrimSpace(progressID)
	if runID == "" || progressID == "" || len(progressID) > maxRunProgressIDLength {
		return domain.OutboxItem{}, errors.Join(storeport.ErrInvalid, errors.New("run progress identity is invalid"))
	}
	if maxMessages <= 0 || maxMessages > 32 {
		return domain.OutboxItem{}, errors.Join(storeport.ErrInvalid, errors.New("run progress limit is invalid"))
	}
	if err := draft.Validate(); err != nil {
		return domain.OutboxItem{}, err
	}
	if draft.Kind != "text" {
		return domain.OutboxItem{}, errors.Join(storeport.ErrInvalid, errors.New("run progress must be text"))
	}
	contentHash, err := runProgressContentHash(draft)
	if err != nil {
		return domain.OutboxItem{}, err
	}

	var committed domain.OutboxItem
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		existingHash, existing, err := findRunProgressByID(ctx, tx, runID, progressID)
		switch {
		case err == nil:
			if existingHash != contentHash {
				return storeport.ErrConflict
			}
			committed = existing
			return nil
		case !errors.Is(err, storeport.ErrNotFound):
			return err
		}

		run, err := scanRun(tx.QueryRowContext(ctx,
			`SELECT `+directRunColumns+` FROM runs WHERE id=?`, runID,
		))
		if err != nil {
			return err
		}
		if run.State != domain.RunRunning || run.LeaseToken == "" || run.LeaseToken != leaseToken {
			return storeport.ErrConflict
		}
		if draft.SessionID != run.SessionID {
			return errors.New("run progress session_id does not match the Run")
		}

		existing, err = findRunProgressByContent(ctx, tx, runID, contentHash)
		switch {
		case err == nil:
			committed = existing
			return nil
		case !errors.Is(err, storeport.ErrNotFound):
			return err
		}

		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT outbox_id)
			FROM run_progress_outputs WHERE run_id=?`, runID).Scan(&count); err != nil {
			return err
		}
		if count >= maxMessages {
			return errors.Join(storeport.ErrCapacity, errors.New("run progress message limit reached"))
		}

		now := time.Now()
		var sequence int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(sequence),0)+1 FROM outbox WHERE session_id=?`,
			run.SessionID,
		).Scan(&sequence); err != nil {
			return err
		}
		outboxID, err := newID("outbox")
		if err != nil {
			return err
		}
		committed = domain.OutboxItem{
			ID: outboxID, RunID: run.ID, SessionID: draft.SessionID,
			ReceiverID: draft.ReceiverID, Kind: draft.Kind,
			Payload: cloneBytes(draft.Payload), Sequence: sequence,
			State: domain.OutboxPending, NextAttempt: now,
			CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(
			id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,
			attempt,lease_token,lease_until,next_attempt_at,receipt_id,
			receipt_time,last_error,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			committed.ID, committed.RunID, committed.SessionID, committed.ReceiverID,
			committed.Kind, []byte(committed.Payload), committed.Sequence, committed.State,
			committed.Attempt, committed.LeaseToken, unixMillis(committed.LeaseUntil),
			unixMillis(committed.NextAttempt), committed.ReceiptID,
			unixMillis(committed.ReceiptTime), committed.LastError,
			unixMillis(committed.CreatedAt), unixMillis(committed.UpdatedAt),
		); err != nil {
			return fmt.Errorf("write Run progress Outbox: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO run_progress_outputs(
			run_id,progress_id,content_hash,outbox_id,created_at
		) VALUES(?,?,?,?,?)`, runID, progressID, contentHash, committed.ID,
			unixMillis(now))
		return err
	})
	return committed, err
}

func findRunProgressByID(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
	progressID string,
) (string, domain.OutboxItem, error) {
	var contentHash, outboxID string
	err := tx.QueryRowContext(ctx, `SELECT content_hash,outbox_id
		FROM run_progress_outputs WHERE run_id=? AND progress_id=?`,
		runID, progressID,
	).Scan(&contentHash, &outboxID)
	if err != nil {
		return "", domain.OutboxItem{}, mapScanError(err)
	}
	item, err := scanOutbox(tx.QueryRowContext(ctx,
		`SELECT `+outboxColumns+` FROM outbox WHERE id=?`, outboxID,
	))
	return contentHash, item, err
}

func findRunProgressByContent(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
	contentHash string,
) (domain.OutboxItem, error) {
	return scanOutbox(tx.QueryRowContext(ctx, `SELECT `+outboxColumns+`
		FROM outbox WHERE id=(SELECT outbox_id FROM run_progress_outputs
			WHERE run_id=? AND content_hash=? ORDER BY created_at LIMIT 1)`,
		runID, contentHash,
	))
}

func runProgressContentHash(draft domain.OutboxDraft) (string, error) {
	encoded, err := json.Marshal(struct {
		SessionID  string          `json:"session_id"`
		ReceiverID string          `json:"receiver_id"`
		Kind       string          `json:"kind"`
		Payload    json.RawMessage `json:"payload"`
	}{
		SessionID: draft.SessionID, ReceiverID: draft.ReceiverID,
		Kind: draft.Kind, Payload: draft.Payload,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
