package sqlite

import (
	"context"
	"database/sql"
	"time"

	"golem_plugin_hermes/internal/domain"
)

const restartedVideoDeliveryError = "plugin restarted during video delivery; automatic retry suppressed"

func recoverOutbox(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	videoCount, err := deadLetterInterruptedVideos(ctx, tx, now)
	if err != nil {
		return 0, err
	}
	otherCount, err := requeueInterruptedOutbox(ctx, tx, now)
	return videoCount + otherCount, err
}

func deadLetterInterruptedVideos(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	where := `kind='video' AND state=?`
	args := []any{domain.OutboxLeased}
	if err := insertSuppressedVideoAttempts(
		ctx, tx, where, args, restartedVideoDeliveryError, now,
	); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE outbox
		SET state=?,lease_token='',lease_until=0,next_attempt_at=0,
			last_error=?,updated_at=? WHERE kind='video' AND state=?`,
		domain.OutboxDeadLetter, restartedVideoDeliveryError, unixMillis(now),
		domain.OutboxLeased)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func requeueInterruptedOutbox(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE outbox
		SET state=?,lease_token='',lease_until=0,next_attempt_at=?,
			last_error='plugin restarted during delivery',updated_at=?
		WHERE state=? AND kind<>'video'`, domain.OutboxRetryWait,
		unixMillis(now), unixMillis(now), domain.OutboxLeased)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
