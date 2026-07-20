package sqlite

import (
	"context"
	"database/sql"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func recoverRuns(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	turnIDs, err := cancelRequestedTurnIDs(ctx, tx)
	if err != nil {
		return 0, err
	}
	for _, turnID := range turnIDs {
		if err := cancelTurn(ctx, tx, turnID, now); err != nil {
			return 0, err
		}
	}
	cancelled, err := tx.ExecContext(ctx, `UPDATE runs
		SET state=?,lease_token='',lease_until=0,updated_at=? WHERE state=?`,
		domain.RunCancelled, unixMillis(now), domain.RunCancelRequested)
	if err != nil {
		return 0, err
	}
	cancelledCount, err := cancelled.RowsAffected()
	if err != nil {
		return 0, err
	}
	runCount, err := requeueInterruptedRuns(ctx, tx, now)
	return cancelledCount + runCount, err
}

func cancelRequestedTurnIDs(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT turn_id FROM runs WHERE state=?`, domain.RunCancelRequested)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var turnID string
		if err := rows.Scan(&turnID); err != nil {
			return nil, err
		}
		result = append(result, turnID)
	}
	return result, rows.Err()
}

func requeueInterruptedRuns(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE runs
		SET state=?,lease_token='',lease_until=0,next_attempt_at=?,
			last_error='plugin restarted',updated_at=? WHERE state IN (?,?,?)`,
		domain.RunRetryWait, unixMillis(now), unixMillis(now),
		domain.RunLeased, domain.RunRunning, domain.RunOrphaned)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
