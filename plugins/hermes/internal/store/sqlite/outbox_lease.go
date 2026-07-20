package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const expiredVideoLeaseError = "video delivery lease expired; automatic retry suppressed"

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
		if err := deadLetterExpiredVideoLeases(ctx, tx, now); err != nil {
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

func deadLetterExpiredVideoLeases(ctx context.Context, tx *sql.Tx, now time.Time) error {
	where := `kind='video' AND attempt>0 AND (
		state IN (?,?) OR (state=? AND lease_until<=?)
	)`
	args := []any{
		domain.OutboxPending, domain.OutboxRetryWait,
		domain.OutboxLeased, unixMillis(now),
	}
	if err := insertSuppressedVideoAttempts(ctx, tx, where, args, expiredVideoLeaseError, now); err != nil {
		return err
	}
	query := `UPDATE outbox SET state=?,lease_token='',lease_until=0,next_attempt_at=0,
		last_error=?,updated_at=? WHERE ` + where
	values := append([]any{domain.OutboxDeadLetter, expiredVideoLeaseError, unixMillis(now)}, args...)
	_, err := tx.ExecContext(ctx, query, values...)
	return err
}

func insertSuppressedVideoAttempts(
	ctx context.Context,
	tx *sql.Tx,
	where string,
	args []any,
	message string,
	now time.Time,
) error {
	query := `INSERT INTO delivery_attempts(
		outbox_id,attempt,outcome,error,receipt_id,receipt_time,created_at
	) SELECT id,attempt,'ambiguous',?,0,0,? FROM outbox WHERE ` + where
	values := append([]any{message, unixMillis(now)}, args...)
	_, err := tx.ExecContext(ctx, query, values...)
	return err
}

func selectNextOutbox(ctx context.Context, tx *sql.Tx, now time.Time) (domain.OutboxItem, error) {
	query := `SELECT ` + outboxColumns + ` FROM outbox o
		WHERE (o.state=? OR (o.state=? AND o.next_attempt_at<=?)
			OR (o.state=? AND o.lease_until<=?))
		AND NOT EXISTS (SELECT 1 FROM outbox active
			WHERE active.session_id=o.session_id AND active.id<>o.id
			AND ((active.state=? AND active.attempt=1)
				OR (active.sequence<o.sequence AND active.state=?)))
		ORDER BY o.created_at,o.session_id,o.sequence LIMIT 1`
	return scanOutbox(tx.QueryRowContext(
		ctx, query, domain.OutboxPending, domain.OutboxRetryWait, unixMillis(now),
		domain.OutboxLeased, unixMillis(now), domain.OutboxLeased, domain.OutboxPending,
	))
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
