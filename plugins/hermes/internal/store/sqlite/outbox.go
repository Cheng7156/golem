package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) MarkOutboxSent(
	ctx context.Context,
	id string,
	leaseToken string,
	receiptID uint64,
	receiptTime time.Time,
) error {
	if receiptID == 0 {
		return errors.Join(storeport.ErrInvalid, errors.New("outbox sent receipt id must be non-zero"))
	}
	if receiptTime.IsZero() {
		receiptTime = time.Now()
	}
	return s.finishOutboxAttempt(ctx, id, leaseToken, "sent", "", receiptID, receiptTime, time.Time{}, false)
}

func (s *Store) MarkOutboxRetry(
	ctx context.Context,
	id string,
	leaseToken string,
	outcome string,
	message string,
	nextAttempt time.Time,
) error {
	outcome = strings.TrimSpace(outcome)
	if outcome != "failed" && outcome != "ambiguous" {
		return errors.New("Outbox retry outcome 必须为 failed 或 ambiguous")
	}
	if nextAttempt.IsZero() {
		return errors.New("Outbox retry 必须提供 nextAttempt")
	}
	return s.finishOutboxAttempt(ctx, id, leaseToken, outcome, message, 0, time.Time{}, nextAttempt, false)
}

func (s *Store) MarkOutboxDeadLetter(
	ctx context.Context,
	id string,
	leaseToken string,
	outcome string,
	message string,
) error {
	outcome = strings.TrimSpace(outcome)
	if outcome != "failed" && outcome != "ambiguous" {
		return errors.New("outbox dead-letter outcome must be failed or ambiguous")
	}
	return s.finishOutboxAttempt(ctx, id, leaseToken, outcome, message, 0, time.Time{}, time.Time{}, true)
}

func (s *Store) finishOutboxAttempt(
	ctx context.Context,
	id string,
	leaseToken string,
	outcome string,
	message string,
	receiptID uint64,
	receiptTime time.Time,
	nextAttempt time.Time,
	deadLetter bool,
) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		item, err := scanOutbox(tx.QueryRowContext(ctx,
			`SELECT `+outboxColumns+` FROM outbox WHERE id=?`,
			id,
		))
		if err != nil {
			return err
		}
		if item.State != domain.OutboxLeased || item.LeaseToken == "" || item.LeaseToken != leaseToken {
			return storeport.ErrConflict
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO delivery_attempts(
				outbox_id,attempt,outcome,error,receipt_id,receipt_time,created_at
			) VALUES(?,?,?,?,?,?,?)
		`,
			item.ID,
			item.Attempt,
			outcome,
			message,
			receiptID,
			unixMillis(receiptTime),
			unixMillis(now),
		); err != nil {
			return err
		}
		target := domain.OutboxRetryWait
		if outcome == "sent" {
			target = domain.OutboxSent
		} else if deadLetter {
			target = domain.OutboxDeadLetter
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE outbox
			SET state=?,lease_token='',lease_until=0,next_attempt_at=?,
			    receipt_id=?,receipt_time=?,last_error=?,updated_at=?
			WHERE id=? AND state=? AND lease_token=?
		`,
			target,
			unixMillis(nextAttempt),
			receiptID,
			unixMillis(receiptTime),
			message,
			unixMillis(now),
			item.ID,
			domain.OutboxLeased,
			leaseToken,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return storeport.ErrConflict
		}
		return nil
	})
}

func (s *Store) GetOutbox(ctx context.Context, id string) (domain.OutboxItem, error) {
	db, err := s.readable()
	if err != nil {
		return domain.OutboxItem{}, err
	}
	return scanOutbox(db.QueryRowContext(ctx,
		`SELECT `+outboxColumns+` FROM outbox WHERE id=?`,
		id,
	))
}

func (s *Store) ListDeliveryAttempts(ctx context.Context, outboxID string) ([]domain.DeliveryAttempt, error) {
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id,outbox_id,attempt,outcome,error,receipt_id,receipt_time,created_at
		FROM delivery_attempts
		WHERE outbox_id=?
		ORDER BY id
	`, outboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.DeliveryAttempt
	for rows.Next() {
		var value domain.DeliveryAttempt
		var receiptTime, createdAt int64
		if err := rows.Scan(
			&value.ID,
			&value.OutboxID,
			&value.Attempt,
			&value.Outcome,
			&value.Error,
			&value.ReceiptID,
			&receiptTime,
			&createdAt,
		); err != nil {
			return nil, err
		}
		value.ReceiptTime = fromUnixMillis(receiptTime)
		value.CreatedAt = fromUnixMillis(createdAt)
		result = append(result, value)
	}
	return result, rows.Err()
}
