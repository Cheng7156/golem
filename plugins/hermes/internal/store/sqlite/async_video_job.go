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

const asyncVideoJobColumns = `
	id,ticket_hash,candidate_id,source_url,media_url,title,invocation_id,auto_close,state,stage,attempt,
	lease_token,lease_until,next_attempt_at,queued,outbox_id,outbox_sequence,
	direct_output_count,failure,created_at,updated_at`

func (s *Store) CreateAsyncVideoJob(
	ctx context.Context,
	job domain.AsyncVideoJob,
) (domain.AsyncVideoJob, bool, error) {
	if err := job.Validate(); err != nil {
		return domain.AsyncVideoJob{}, false, errors.Join(storeport.ErrInvalid, err)
	}
	now := job.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	job.State = domain.AsyncVideoJobPending
	job.Stage = "queued"
	job.CreatedAt, job.UpdatedAt = now, now
	var stored domain.AsyncVideoJob
	created := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		existing, err := scanAsyncVideoJob(tx.QueryRowContext(ctx,
			`SELECT `+asyncVideoJobColumns+` FROM async_video_jobs WHERE id=?`, job.ID))
		if err == nil {
			if !existing.SameInvocation(job) {
				return storeport.ErrConflict
			}
			stored = existing
			return nil
		}
		if !errors.Is(err, storeport.ErrNotFound) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO async_video_jobs(
			id,ticket_hash,candidate_id,source_url,media_url,title,invocation_id,auto_close,state,stage,attempt,
			lease_token,lease_until,next_attempt_at,queued,outbox_id,outbox_sequence,
			direct_output_count,failure,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, job.ID, job.TicketHash,
			job.CandidateID, job.SourceURL, job.MediaURL, job.Title, job.InvocationID,
			job.AutoClose, job.State, job.Stage, 0,
			"", 0, 0, 0, "", 0, 0, "", unixMillis(now), unixMillis(now))
		if err == nil {
			stored, created = job, true
		}
		return err
	})
	return stored, created, err
}

func (s *Store) UpdateAsyncVideoJobStage(
	ctx context.Context,
	id, token, stage string,
) error {
	stage = strings.TrimSpace(stage)
	if stage == "" || len(stage) > 64 {
		return storeport.ErrInvalid
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE async_video_jobs SET
			stage=?,updated_at=? WHERE id=? AND state=? AND lease_token=?`, stage,
			unixMillis(time.Now()), id, domain.AsyncVideoJobRunning, token)
		return requireOneAsyncVideoJobRow(result, err)
	})
}

func (s *Store) GetAsyncVideoJob(
	ctx context.Context,
	id string,
) (domain.AsyncVideoJob, error) {
	db, err := s.readable()
	if err != nil {
		return domain.AsyncVideoJob{}, err
	}
	return scanAsyncVideoJob(db.QueryRowContext(ctx,
		`SELECT `+asyncVideoJobColumns+` FROM async_video_jobs WHERE id=?`, id))
}

func (s *Store) ListRunnableAsyncVideoJobs(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]domain.AsyncVideoJob, error) {
	if limit <= 0 || limit > 100 {
		return nil, storeport.ErrInvalid
	}
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+asyncVideoJobColumns+`
		FROM async_video_jobs WHERE
		(state=? AND next_attempt_at<=?) OR (state=? AND lease_until<=?)
		ORDER BY created_at LIMIT ?`, domain.AsyncVideoJobPending, unixMillis(now),
		domain.AsyncVideoJobRunning, unixMillis(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.AsyncVideoJob
	for rows.Next() {
		job, scanErr := scanAsyncVideoJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *Store) LeaseAsyncVideoJob(
	ctx context.Context,
	id string,
	now time.Time,
	duration time.Duration,
) (domain.AsyncVideoJob, error) {
	if strings.TrimSpace(id) == "" || duration <= 0 {
		return domain.AsyncVideoJob{}, storeport.ErrInvalid
	}
	var leased domain.AsyncVideoJob
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		job, err := scanAsyncVideoJob(tx.QueryRowContext(ctx,
			`SELECT `+asyncVideoJobColumns+` FROM async_video_jobs WHERE id=?`, id))
		if err != nil {
			return err
		}
		eligible := job.State == domain.AsyncVideoJobPending && !job.NextAttemptAt.After(now)
		eligible = eligible || job.State == domain.AsyncVideoJobRunning && !job.LeaseUntil.After(now)
		if !eligible {
			return storeport.ErrConflict
		}
		token, err := newID("avlease")
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE async_video_jobs SET
			state=?,attempt=attempt+1,lease_token=?,lease_until=?,updated_at=?
			WHERE id=? AND state=? AND lease_token=?`, domain.AsyncVideoJobRunning,
			token, unixMillis(now.Add(duration)), unixMillis(now), job.ID, job.State, job.LeaseToken)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return errors.Join(err, storeport.ErrConflict)
		}
		job.State = domain.AsyncVideoJobRunning
		job.Attempt++
		job.LeaseToken = token
		job.LeaseUntil = now.Add(duration)
		job.UpdatedAt = now
		leased = job
		return nil
	})
	return leased, err
}

func (s *Store) ExtendAsyncVideoJobLease(
	ctx context.Context,
	id, token string,
	until time.Time,
) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE async_video_jobs SET
			lease_until=?,updated_at=? WHERE id=? AND state=? AND lease_token=?`,
			unixMillis(until), unixMillis(time.Now()), id, domain.AsyncVideoJobRunning, token)
		return requireOneAsyncVideoJobRow(result, err)
	})
}

func (s *Store) FinishAsyncVideoJob(
	ctx context.Context,
	id, token string,
	result domain.AsyncDirectOutputResult,
	failure string,
) error {
	state := domain.AsyncVideoJobWaitingDelivery
	if strings.TrimSpace(failure) != "" {
		state = domain.AsyncVideoJobFailed
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		dbResult, err := tx.ExecContext(ctx, `UPDATE async_video_jobs SET
			state=?,stage=?,lease_token='',lease_until=0,queued=?,outbox_id=?,outbox_sequence=?,
			direct_output_count=?,failure=?,updated_at=?
			WHERE id=? AND state=? AND lease_token=?`, state, string(state), result.Queued, result.OutboxID,
			result.Sequence, result.DirectOutputCount, failure, unixMillis(time.Now()),
			id, domain.AsyncVideoJobRunning, token)
		return requireOneAsyncVideoJobRow(dbResult, err)
	})
}

func (s *Store) RequeueAsyncVideoJob(
	ctx context.Context,
	id, token, failure string,
	nextAttempt time.Time,
) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE async_video_jobs SET
			state=?,stage='retry_wait',lease_token='',lease_until=0,next_attempt_at=?,failure=?,updated_at=?
			WHERE id=? AND state=? AND lease_token=?`, domain.AsyncVideoJobPending,
			unixMillis(nextAttempt), failure, unixMillis(time.Now()), id,
			domain.AsyncVideoJobRunning, token)
		return requireOneAsyncVideoJobRow(result, err)
	})
}

func (s *Store) RememberAsyncVideoURLs(
	ctx context.Context,
	ticketHash string,
	urls []string,
	allowedUntil time.Time,
) error {
	if strings.TrimSpace(ticketHash) == "" || allowedUntil.IsZero() {
		return storeport.ErrInvalid
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := unixMillis(time.Now())
		for _, rawURL := range urls {
			value := strings.TrimSpace(rawURL)
			if value == "" || len(value) > 4096 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO async_video_urls(
				ticket_hash,normalized_url,allowed_until,created_at
			) VALUES(?,?,?,?) ON CONFLICT(ticket_hash,normalized_url) DO UPDATE SET
				allowed_until=MAX(allowed_until,excluded.allowed_until)`, ticketHash,
				value, unixMillis(allowedUntil), now); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM async_video_urls WHERE allowed_until<=?`, now)
		return err
	})
}

func (s *Store) AsyncVideoURLAllowed(
	ctx context.Context,
	ticketHash, rawURL string,
	now time.Time,
) (bool, error) {
	db, err := s.readable()
	if err != nil {
		return false, err
	}
	var count int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM async_video_urls
		WHERE ticket_hash=? AND normalized_url=? AND allowed_until>?`, ticketHash,
		strings.TrimSpace(rawURL), unixMillis(now)).Scan(&count)
	return count == 1, err
}

func (s *Store) DeleteAsyncVideoURLs(ctx context.Context, ticketHash string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM async_video_urls WHERE ticket_hash=?`, ticketHash)
		return err
	})
}

func requireOneAsyncVideoJobRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.Join(err, storeport.ErrConflict)
	}
	return nil
}

func scanAsyncVideoJob(scanner interface{ Scan(...any) error }) (domain.AsyncVideoJob, error) {
	var job domain.AsyncVideoJob
	var leaseUntil, nextAttemptAt, createdAt, updatedAt int64
	err := scanner.Scan(&job.ID, &job.TicketHash, &job.CandidateID, &job.SourceURL,
		&job.MediaURL, &job.Title, &job.InvocationID, &job.AutoClose, &job.State,
		&job.Stage, &job.Attempt, &job.LeaseToken,
		&leaseUntil, &nextAttemptAt, &job.Result.Queued, &job.Result.OutboxID,
		&job.Result.Sequence, &job.Result.DirectOutputCount, &job.Failure,
		&createdAt, &updatedAt)
	if err != nil {
		return domain.AsyncVideoJob{}, mapScanError(err)
	}
	job.LeaseUntil = fromUnixMillis(leaseUntil)
	job.NextAttemptAt = fromUnixMillis(nextAttemptAt)
	job.CreatedAt = fromUnixMillis(createdAt)
	job.UpdatedAt = fromUnixMillis(updatedAt)
	return job, nil
}
