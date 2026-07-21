package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type ObservationMaintenanceStatus struct {
	Pending               int64
	Leased                int64
	RetryWait             int64
	Acked                 int64
	Conflict              int64
	DeadLetter            int64
	BlockedConversations  int64
	OldestTerminalUpdated time.Time
	Blocked               []ObservationBlockage
}

type ObservationBlockage struct {
	ConversationID  string
	State           domain.ContextOutboxState
	ConversationSeq int64
	LastError       string
	UpdatedAt       time.Time
}

func (s *Store) ObservationMaintenanceStatus(ctx context.Context) (ObservationMaintenanceStatus, error) {
	db, err := s.readable()
	if err != nil {
		return ObservationMaintenanceStatus{}, err
	}
	var result ObservationMaintenanceStatus
	rows, err := db.QueryContext(ctx, `SELECT state,COUNT(*) FROM context_outbox GROUP BY state`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var state domain.ContextOutboxState
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return result, err
		}
		switch state {
		case domain.ContextPending:
			result.Pending = count
		case domain.ContextLeased:
			result.Leased = count
		case domain.ContextRetryWait:
			result.RetryWait = count
		case domain.ContextAcked:
			result.Acked = count
		case domain.ContextConflict:
			result.Conflict = count
		case domain.ContextDeadLetter:
			result.DeadLetter = count
		}
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	var oldest int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT conversation_id),COALESCE(MIN(updated_at),0)
		FROM context_outbox WHERE state IN (?,?)`, domain.ContextConflict, domain.ContextDeadLetter).
		Scan(&result.BlockedConversations, &oldest); err != nil {
		return result, err
	}
	if oldest > 0 {
		result.OldestTerminalUpdated = fromUnixMillis(oldest)
	}
	rows, err = db.QueryContext(ctx, `SELECT c.conversation_id,c.state,c.conversation_seq,c.last_error,c.updated_at
		FROM context_outbox c WHERE c.state IN (?,?) AND c.conversation_seq=(
			SELECT MIN(first.conversation_seq) FROM context_outbox first
			WHERE first.conversation_id=c.conversation_id AND first.state IN (?,?)
		) ORDER BY c.updated_at,c.conversation_id LIMIT 10`, domain.ContextConflict, domain.ContextDeadLetter,
		domain.ContextConflict, domain.ContextDeadLetter)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var blockage ObservationBlockage
		var updatedAt int64
		if err := rows.Scan(&blockage.ConversationID, &blockage.State, &blockage.ConversationSeq,
			&blockage.LastError, &updatedAt); err != nil {
			_ = rows.Close()
			return result, err
		}
		blockage.UpdatedAt = fromUnixMillis(updatedAt)
		result.Blocked = append(result.Blocked, blockage)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, nil
}

// RepairObservationConversation validates and requeues the earliest terminal
// range without changing its payload or skipping any conversation sequence.
func (s *Store) RepairObservationConversation(
	ctx context.Context,
	conversationID string,
	now time.Time,
) (int64, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || now.IsZero() {
		return 0, storeport.ErrInvalid
	}
	var repaired int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+contextOutboxColumns+` FROM context_outbox
			WHERE conversation_id=? AND state<>? ORDER BY conversation_seq`, conversationID, domain.ContextAcked)
		if err != nil {
			return err
		}
		var first, last int64
		started := false
		for rows.Next() {
			item, scanErr := scanContextOutbox(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			terminal := item.State == domain.ContextConflict || item.State == domain.ContextDeadLetter
			if !started {
				if !terminal {
					_ = rows.Close()
					return storeport.ErrConflict
				}
				started = true
				first = item.ConversationSeq
			} else if !terminal {
				break
			}
			if item.ConversationSeq != last+1 && last != 0 {
				_ = rows.Close()
				return fmt.Errorf("observation terminal range has a sequence gap: %w", storeport.ErrConflict)
			}
			if err := validateStoredObservation(item); err != nil {
				_ = rows.Close()
				return err
			}
			last = item.ConversationSeq
			repaired++
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !started {
			return storeport.ErrNotFound
		}
		result, err := tx.ExecContext(ctx, `UPDATE context_outbox SET state=?,attempt=0,lease_token='',
			lease_until=0,next_attempt_at=?,last_error='',updated_at=? WHERE conversation_id=?
			AND conversation_seq BETWEEN ? AND ? AND state IN (?,?)`, domain.ContextRetryWait,
			unixMillis(now), unixMillis(now), conversationID, first, last,
			domain.ContextConflict, domain.ContextDeadLetter)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != repaired {
			return storeport.ErrConflict
		}
		return nil
	})
	return repaired, err
}

func validateStoredObservation(item domain.ContextOutboxItem) error {
	observation := item.Observation
	originalHash := observation.PayloadHash
	if observation.ConversationID != item.ConversationID ||
		observation.ConversationSeq != item.ConversationSeq || observation.ObservationID != item.ID {
		return fmt.Errorf("stored observation identity mismatch: %w", storeport.ErrConflict)
	}
	if err := domain.FinalizeObservationHash(&observation); err != nil {
		return err
	}
	if observation.PayloadHash != originalHash {
		return fmt.Errorf("stored observation payload hash mismatch: %w", storeport.ErrConflict)
	}
	return nil
}

// PruneAckedObservations removes old acknowledged rows only when a later row
// remains as that conversation's sequence watermark. Rows referenced by an
// active Run are retained. Terminal and unacknowledged rows are never pruned.
func (s *Store) PruneAckedObservations(
	ctx context.Context,
	before time.Time,
	limit int,
) (int64, error) {
	if before.IsZero() || limit <= 0 || limit > 10000 {
		return 0, storeport.ErrInvalid
	}
	var pruned int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT c.id FROM context_outbox c
			WHERE c.state=? AND c.updated_at<?
			AND EXISTS (SELECT 1 FROM context_outbox newer WHERE newer.conversation_id=c.conversation_id
				AND newer.conversation_seq>c.conversation_seq)
			AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.current_observation_id=c.id
				AND r.state IN (?,?,?,?,?,?))
			ORDER BY c.updated_at,c.conversation_id,c.conversation_seq LIMIT ?`, domain.ContextAcked,
			unixMillis(before), domain.RunQueued, domain.RunLeased, domain.RunRunning, domain.RunRetryWait,
			domain.RunCancelRequested, domain.RunOrphaned, limit)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
		arguments := make([]any, len(ids))
		for index := range ids {
			arguments[index] = ids[index]
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM context_outbox WHERE state=? AND id IN (`+placeholders+`)`,
			append([]any{domain.ContextAcked}, arguments...)...)
		if err != nil {
			return err
		}
		pruned, err = result.RowsAffected()
		return err
	})
	return pruned, err
}
