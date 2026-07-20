package sqlite

import (
	"context"
	"database/sql"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func (s *Store) Recover(ctx context.Context, now time.Time) (domain.RecoveryResult, error) {
	var recovered domain.RecoveryResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		recovered.RunsRecovered, err = recoverRuns(ctx, tx, now)
		if err != nil {
			return err
		}
		recovered.OutboxRecovered, err = recoverOutbox(ctx, tx, now)
		return err
	})
	return recovered, err
}
