package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

// ReserveAmbientReply fences model execution against the durable visible-reply
// budget. The reservation is converted to consumed only by the same transaction
// that commits a visible outbox item.
func (s *Store) ReserveAmbientReply(
	ctx context.Context,
	runID string,
	now time.Time,
	cooldown time.Duration,
	window time.Duration,
	maxReplies int,
) (bool, error) {
	if runID == "" || now.IsZero() || cooldown <= 0 || window <= 0 || maxReplies <= 0 {
		return false, storeport.ErrInvalid
	}
	allowed := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var sessionID string
		var trigger domain.TriggerKind
		var state domain.RunState
		if err := tx.QueryRowContext(ctx,
			`SELECT session_id,trigger_kind,state FROM runs WHERE id=?`, runID,
		).Scan(&sessionID, &trigger, &state); err != nil {
			return mapScanError(err)
		}
		if trigger != domain.TriggerAmbient {
			allowed = true
			return nil
		}
		if state != domain.RunRunning {
			return storeport.ErrConflict
		}

		var existingState string
		err := tx.QueryRowContext(ctx,
			`SELECT state FROM ambient_reply_budget WHERE run_id=?`, runID,
		).Scan(&existingState)
		switch {
		case err == nil:
			allowed = existingState == "reserved" || existingState == "consumed"
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}

		// Clear abandoned reservations whose Runs are already terminal. Retain
		// consumed rows for 30 days so normal quota increases still have history.
		if _, err := tx.ExecContext(ctx, `DELETE FROM ambient_reply_budget
			WHERE state='reserved' AND run_id IN (
				SELECT id FROM runs WHERE state IN (?,?,?)
			)`, domain.RunSucceeded, domain.RunFailed, domain.RunCancelled); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ambient_reply_budget
			WHERE state='consumed' AND consumed_at<?`, unixMillis(now.Add(-30*24*time.Hour))); err != nil {
			return err
		}

		var activeReservations int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ambient_reply_budget
			WHERE session_id=? AND state='reserved'`, sessionID).Scan(&activeReservations); err != nil {
			return err
		}
		if activeReservations > 0 {
			return nil
		}
		windowStart := unixMillis(now.Add(-window))
		var consumed int
		var latest int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(consumed_at),0)
			FROM ambient_reply_budget
			WHERE session_id=? AND state='consumed' AND consumed_at>?`,
			sessionID, windowStart).Scan(&consumed, &latest); err != nil {
			return err
		}
		if consumed >= maxReplies || (latest > 0 && now.Sub(fromUnixMillis(latest)) < cooldown) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ambient_reply_budget(
			run_id,session_id,state,reserved_at,consumed_at
		) VALUES(?,?, 'reserved',?,0)`, runID, sessionID, unixMillis(now)); err != nil {
			return err
		}
		allowed = true
		return nil
	})
	return allowed, err
}

func settleAmbientReplyBudget(
	ctx context.Context,
	tx *sql.Tx,
	run domain.Run,
	visible bool,
	now time.Time,
) error {
	if run.TriggerKind != domain.TriggerAmbient {
		return nil
	}
	if !visible {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM ambient_reply_budget WHERE run_id=? AND state='reserved'`, run.ID)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE ambient_reply_budget
		SET state='consumed',consumed_at=? WHERE run_id=? AND state='reserved'`,
		unixMillis(now), run.ID)
	return err
}

func releaseAmbientReplyBudget(ctx context.Context, tx *sql.Tx, runID string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM ambient_reply_budget WHERE run_id=? AND state='reserved'`, runID)
	return err
}
