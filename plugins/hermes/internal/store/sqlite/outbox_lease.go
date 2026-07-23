package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const (
	expiredVideoSendingError = "video send was started but its receipt is unknown after lease expiry"
	expiredVideoLeasedError  = "video send was not started before its lease expired"
)

func (s *Store) LeaseNextOutbox(
	ctx context.Context,
	now time.Time,
	leaseDuration time.Duration,
) (domain.OutboxItem, error) {
	if leaseDuration <= 0 {
		return domain.OutboxItem{}, errors.New("Outbox lease duration must be positive")
	}
	var leased domain.OutboxItem
	found := true
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := resolveExpiredVideoLeases(ctx, tx, now); err != nil {
			return err
		}
		value, err := selectNextOutbox(ctx, tx, now)
		if errors.Is(err, storeport.ErrNotFound) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		leased, err = leaseOutbox(ctx, tx, value, now, leaseDuration)
		return err
	})
	if err == nil && !found {
		return domain.OutboxItem{}, storeport.ErrNotFound
	}
	return leased, err
}

func resolveExpiredVideoLeases(ctx context.Context, tx *sql.Tx, now time.Time) error {
	sendingWhere := `kind='video' AND state=? AND lease_until<=?`
	sendingArgs := []any{domain.OutboxSending, unixMillis(now)}
	if err := insertVideoAttempt(ctx, tx, sendingWhere, sendingArgs, "ambiguous", expiredVideoSendingError, now); err != nil {
		return err
	}
	if err := updateAsyncVideoJobsForOutboxWhere(ctx, tx, sendingWhere, sendingArgs,
		domain.AsyncVideoJobAmbiguous, expiredVideoSendingError, now); err != nil {
		return err
	}
	values := append([]any{domain.OutboxAmbiguous, expiredVideoSendingError, unixMillis(now)}, sendingArgs...)
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,lease_token='',lease_until=0,
		next_attempt_at=0,last_error=?,updated_at=? WHERE `+sendingWhere, values...); err != nil {
		return err
	}

	leasedWhere := `kind='video' AND state=? AND lease_until<=?`
	leasedArgs := []any{domain.OutboxLeased, unixMillis(now)}
	if err := insertVideoAttempt(ctx, tx, leasedWhere, leasedArgs, "not_started", expiredVideoLeasedError, now); err != nil {
		return err
	}
	values = append([]any{domain.OutboxRetryWait, unixMillis(now), expiredVideoLeasedError,
		unixMillis(now)}, leasedArgs...)
	_, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,lease_token='',lease_until=0,
		next_attempt_at=?,last_error=?,updated_at=? WHERE `+leasedWhere, values...)
	return err
}

func insertVideoAttempt(
	ctx context.Context,
	tx *sql.Tx,
	where string,
	args []any,
	outcome string,
	message string,
	now time.Time,
) error {
	query := `INSERT INTO delivery_attempts(
		outbox_id,attempt,outcome,error,receipt_id,receipt_time,created_at
	) SELECT id,attempt,?,?,0,0,? FROM outbox WHERE ` + where
	values := append([]any{outcome, message, unixMillis(now)}, args...)
	_, err := tx.ExecContext(ctx, query, values...)
	return err
}

func selectNextOutbox(ctx context.Context, tx *sql.Tx, now time.Time) (domain.OutboxItem, error) {
	query := `SELECT ` + outboxColumns + ` FROM outbox o
		WHERE (o.state=? OR (o.state=? AND o.next_attempt_at<=?)
			OR (o.state=? AND o.lease_until<=?)
			OR (o.kind<>'video' AND o.state=? AND o.lease_until<=?))
		AND NOT EXISTS (SELECT 1 FROM outbox active
			WHERE active.session_id=o.session_id AND active.id<>o.id
			AND ((active.state IN (?,?) AND active.attempt=1)
				OR (active.sequence<o.sequence AND active.state=?)))
		ORDER BY o.created_at,o.session_id,o.sequence LIMIT 1`
	return scanOutbox(tx.QueryRowContext(
		ctx, query, domain.OutboxPending, domain.OutboxRetryWait, unixMillis(now),
		domain.OutboxLeased, unixMillis(now), domain.OutboxSending, unixMillis(now),
		domain.OutboxLeased, domain.OutboxSending, domain.OutboxPending,
	))
}

func updateAsyncVideoJobsForOutboxWhere(
	ctx context.Context,
	tx *sql.Tx,
	where string,
	args []any,
	state domain.AsyncVideoJobState,
	message string,
	now time.Time,
) error {
	query := `UPDATE async_video_jobs SET state=?,stage=?,failure=?,updated_at=?
		WHERE outbox_id IN (SELECT id FROM outbox WHERE ` + where + `)
		AND state IN (?,?)`
	values := append([]any{state, state, message, unixMillis(now)}, args...)
	values = append(values, domain.AsyncVideoJobWaitingDelivery, domain.AsyncVideoJobCompleted)
	_, err := tx.ExecContext(ctx, query, values...)
	return err
}

func leaseOutbox(
	ctx context.Context,
	tx *sql.Tx,
	value domain.OutboxItem,
	now time.Time,
	duration time.Duration,
) (domain.OutboxItem, error) {
	token, err := newID("outlease")
	if err != nil {
		return domain.OutboxItem{}, err
	}
	until := now.Add(duration)
	result, err := tx.ExecContext(ctx, `UPDATE outbox
		SET state=?,lease_token=?,lease_until=?,attempt=attempt+1,updated_at=?
		WHERE id=? AND state=?`, domain.OutboxLeased, token, unixMillis(until),
		unixMillis(now), value.ID, value.State)
	if err != nil {
		return domain.OutboxItem{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return domain.OutboxItem{}, errors.Join(err, storeport.ErrConflict)
	}
	value.State, value.LeaseToken, value.LeaseUntil = domain.OutboxLeased, token, until
	value.Attempt++
	value.UpdatedAt = now
	return value, nil
}
