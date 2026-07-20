package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func (s *Store) RevokeAsyncDeliveries(
	ctx context.Context,
	profile string,
	producerEpoch string,
	hermesSessionID string,
) (int64, error) {
	if profile == "" || producerEpoch == "" || hermesSessionID == "" {
		return 0, errors.New("async delivery revocation has an empty binding")
	}
	var count int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE async_delivery_tickets SET state=?,last_error=?,updated_at=?
			WHERE profile=? AND producer_epoch=? AND hermes_session_id=? AND state=?
		`, domain.AsyncDeliveryRevoked, "session revoked", unixMillis(time.Now()),
			profile, producerEpoch, hermesSessionID, domain.AsyncDeliveryPending)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}

func (s *Store) ReconcileAsyncDeliveries(
	ctx context.Context,
	profile string,
	producerEpoch string,
) (int64, error) {
	if profile == "" || producerEpoch == "" {
		return 0, errors.New("async delivery reconciliation has an empty binding")
	}
	var count int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE async_delivery_tickets SET state=?,last_error=?,updated_at=?
			WHERE profile=? AND producer_epoch<>? AND state=?
		`, domain.AsyncDeliveryAbandoned, "superseded producer epoch",
			unixMillis(time.Now()), profile, producerEpoch, domain.AsyncDeliveryPending)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}
