package sqlite

import (
	"context"
	"database/sql"
	"time"

	"golem_plugin_hermes/internal/domain"
)

const (
	restartedVideoSendingError = "plugin restarted after video send started; receipt is unknown"
	restartedVideoLeasedError  = "plugin restarted before video send started"
)

func recoverOutbox(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	videoCount, err := recoverInterruptedVideos(ctx, tx, now)
	if err != nil {
		return 0, err
	}
	otherCount, err := requeueInterruptedOutbox(ctx, tx, now)
	return videoCount + otherCount, err
}

func recoverInterruptedVideos(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	sendingWhere := `kind='video' AND state=?`
	sendingArgs := []any{domain.OutboxSending}
	if err := insertVideoAttempt(ctx, tx, sendingWhere, sendingArgs, "ambiguous", restartedVideoSendingError, now); err != nil {
		return 0, err
	}
	if err := updateAsyncVideoJobsForOutboxWhere(ctx, tx, sendingWhere, sendingArgs,
		domain.AsyncVideoJobAmbiguous, restartedVideoSendingError, now); err != nil {
		return 0, err
	}
	sending, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,lease_token='',lease_until=0,
		next_attempt_at=0,last_error=?,updated_at=? WHERE `+sendingWhere,
		domain.OutboxAmbiguous, restartedVideoSendingError, unixMillis(now), domain.OutboxSending)
	if err != nil {
		return 0, err
	}
	sendingCount, err := sending.RowsAffected()
	if err != nil {
		return 0, err
	}

	leasedWhere := `kind='video' AND state=?`
	leasedArgs := []any{domain.OutboxLeased}
	if err := insertVideoAttempt(ctx, tx, leasedWhere, leasedArgs, "not_started", restartedVideoLeasedError, now); err != nil {
		return 0, err
	}
	leased, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,lease_token='',lease_until=0,
		next_attempt_at=?,last_error=?,updated_at=? WHERE `+leasedWhere,
		domain.OutboxRetryWait, unixMillis(now), restartedVideoLeasedError, unixMillis(now), domain.OutboxLeased)
	if err != nil {
		return 0, err
	}
	leasedCount, err := leased.RowsAffected()
	return sendingCount + leasedCount, err
}

func requeueInterruptedOutbox(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE outbox
		SET state=?,lease_token='',lease_until=0,next_attempt_at=?,
			last_error='plugin restarted during delivery',updated_at=?
		WHERE state IN (?,?) AND kind<>'video'`, domain.OutboxRetryWait,
		unixMillis(now), unixMillis(now), domain.OutboxLeased, domain.OutboxSending)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
