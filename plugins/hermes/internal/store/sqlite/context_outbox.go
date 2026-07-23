package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const contextOutboxColumns = `
id,conversation_id,accept_seq,conversation_seq,event_id,payload_hash,observation_json,state,attempt,
lease_token,lease_until,next_attempt_at,last_error,created_at,updated_at
`

func (s *Store) LeaseNextObservationBatch(
	ctx context.Context,
	now time.Time,
	leaseDuration time.Duration,
	limit int,
) (domain.ObservationBatch, error) {
	if leaseDuration <= 0 || limit <= 0 {
		return domain.ObservationBatch{}, storeport.ErrInvalid
	}
	var batch domain.ObservationBatch
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+contextOutboxColumns+`
			FROM context_outbox c WHERE state NOT IN (?,?,?) AND EXISTS (
				SELECT 1 FROM turns routed WHERE routed.event_id=c.event_id
					AND routed.state NOT IN (?,?,?)
			) AND NOT EXISTS (
				SELECT 1 FROM context_outbox p WHERE p.conversation_id=c.conversation_id
				AND p.conversation_seq<c.conversation_seq AND p.state<>?
			) ORDER BY created_at,conversation_id LIMIT 128`, domain.ContextAcked,
			domain.ContextConflict, domain.ContextDeadLetter, domain.TurnAccepted,
			domain.TurnOrdered, domain.TurnRouted, domain.ContextAcked)
		if err != nil {
			return err
		}
		var head domain.ContextOutboxItem
		found := false
		for rows.Next() {
			candidate, scanErr := scanContextOutbox(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			if contextItemEligible(candidate, now) {
				head, found = candidate, true
				break
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !found {
			return storeport.ErrNotFound
		}
		rows, err = tx.QueryContext(ctx, `SELECT `+contextOutboxColumns+`
			FROM context_outbox c WHERE conversation_id=? AND state NOT IN (?,?,?) AND EXISTS (
				SELECT 1 FROM turns routed WHERE routed.event_id=c.event_id
					AND routed.state NOT IN (?,?,?)
			)
			ORDER BY conversation_seq LIMIT ?`, head.ConversationID,
			domain.ContextAcked, domain.ContextConflict, domain.ContextDeadLetter,
			domain.TurnAccepted, domain.TurnOrdered, domain.TurnRouted, limit)
		if err != nil {
			return err
		}
		var items []domain.ContextOutboxItem
		for rows.Next() {
			item, scanErr := scanContextOutbox(rows)
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			if !contextItemEligible(item, now) {
				break
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(items) == 0 {
			return storeport.ErrNotFound
		}
		leaseToken, err := newID("ctxlease")
		if err != nil {
			return err
		}
		for _, item := range items {
			result, updateErr := tx.ExecContext(ctx, `UPDATE context_outbox SET state=?,attempt=attempt+1,
				lease_token=?,lease_until=?,updated_at=? WHERE id=? AND state=?`,
				domain.ContextLeased, leaseToken, unixMillis(now.Add(leaseDuration)), unixMillis(now),
				item.ID, item.State)
			if updateErr != nil {
				return updateErr
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return storeport.ErrConflict
			}
			batch.Observations = append(batch.Observations, item.Observation)
			batch.Attempt = max(batch.Attempt, item.Attempt+1)
		}
		batch.ConversationID = head.ConversationID
		batch.FirstConversationSeq = items[0].ConversationSeq
		batch.LastConversationSeq = items[len(items)-1].ConversationSeq
		batch.LeaseToken = leaseToken
		if err := domain.FinalizeBatchHash(&batch); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(batch.BatchHash))
		batch.BatchID = "batch_v1:" + batch.BatchHash
		batch.RequestID = "req_" + hex.EncodeToString(digest[:16])
		return nil
	})
	return batch, err
}

func (s *Store) MarkObservationBatchTerminal(ctx context.Context, batch domain.ObservationBatch,
	state domain.ContextOutboxState, message string) error {
	if state != domain.ContextConflict && state != domain.ContextDeadLetter {
		return storeport.ErrInvalid
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE context_outbox SET state=?,lease_token='',lease_until=0,
			last_error=?,updated_at=? WHERE conversation_id=? AND conversation_seq BETWEEN ? AND ?
			AND state=? AND lease_token=?`, state, message, unixMillis(time.Now()), batch.ConversationID,
			batch.FirstConversationSeq, batch.LastConversationSeq, domain.ContextLeased, batch.LeaseToken)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != int64(len(batch.Observations)) {
			return storeport.ErrConflict
		}
		return nil
	})
}

func contextItemEligible(item domain.ContextOutboxItem, now time.Time) bool {
	switch item.State {
	case domain.ContextPending:
		return true
	case domain.ContextRetryWait:
		return !item.NextAttempt.After(now)
	case domain.ContextLeased:
		return !item.LeaseUntil.After(now)
	default:
		return false
	}
}

func (s *Store) MarkObservationBatchAcked(ctx context.Context, batch domain.ObservationBatch, ack domain.ObservationAck) error {
	if ack.Status != "committed" || ack.BatchID != batch.BatchID || ack.BatchHash != batch.BatchHash ||
		ack.RequestID != batch.RequestID || ack.ConversationID != batch.ConversationID ||
		ack.DurableThroughConversationSeq < batch.LastConversationSeq || len(ack.Gaps) != 0 {
		return storeport.ErrConflict
	}
	if len(ack.Items) != 0 {
		if len(ack.Items) != len(batch.Observations) {
			return storeport.ErrConflict
		}
		for index, item := range ack.Items {
			if item.ObservationID != batch.Observations[index].ObservationID ||
				(item.Status != "committed" && item.Status != "duplicate" && item.Status != "ignored") {
				return storeport.ErrConflict
			}
		}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE context_outbox SET state=?,lease_token='',lease_until=0,
			next_attempt_at=0,last_error='',updated_at=? WHERE conversation_id=? AND conversation_seq<=?
			AND state=? AND lease_token=?`, domain.ContextAcked, unixMillis(time.Now()), batch.ConversationID,
			ack.DurableThroughConversationSeq, domain.ContextLeased, batch.LeaseToken)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != int64(len(batch.Observations)) {
			return storeport.ErrConflict
		}
		return nil
	})
}

func (s *Store) MarkObservationBatchRetry(ctx context.Context, batch domain.ObservationBatch, message string, next time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE context_outbox SET state=?,lease_token='',lease_until=0,
			next_attempt_at=?,last_error=?,updated_at=? WHERE conversation_id=? AND conversation_seq BETWEEN ? AND ?
			AND state=? AND lease_token=?`, domain.ContextRetryWait, unixMillis(next), message, unixMillis(time.Now()),
			batch.ConversationID, batch.FirstConversationSeq, batch.LastConversationSeq,
			domain.ContextLeased, batch.LeaseToken)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != int64(len(batch.Observations)) {
			return storeport.ErrConflict
		}
		return nil
	})
}

func (s *Store) ObservationContextReady(ctx context.Context, conversationID string, requiredSeq int64) (bool, error) {
	if conversationID == "" || requiredSeq <= 0 {
		return true, nil
	}
	db, err := s.readable()
	if err != nil {
		return false, err
	}
	var pending int
	var terminal int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_outbox WHERE conversation_id=?
		AND conversation_seq<=? AND state IN (?,?)`, conversationID, requiredSeq,
		domain.ContextConflict, domain.ContextDeadLetter).Scan(&terminal); err != nil {
		return false, err
	}
	if terminal > 0 {
		return false, storeport.ErrConflict
	}
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_outbox WHERE conversation_id=?
		AND conversation_seq<=? AND state<>?`, conversationID, requiredSeq, domain.ContextAcked).Scan(&pending)
	return pending == 0, err
}

func (s *Store) GetContextOutboxByEvent(ctx context.Context, eventID string) (domain.ContextOutboxItem, error) {
	db, err := s.readable()
	if err != nil {
		return domain.ContextOutboxItem{}, err
	}
	return scanContextOutbox(db.QueryRowContext(ctx, `SELECT `+contextOutboxColumns+` FROM context_outbox WHERE event_id=?`, eventID))
}

func scanContextOutbox(row scanner) (domain.ContextOutboxItem, error) {
	var item domain.ContextOutboxItem
	var observationJSON []byte
	var payloadHash string
	var leaseUntil, nextAttempt, createdAt, updatedAt int64
	if err := row.Scan(&item.ID, &item.ConversationID, &item.AcceptSeq, &item.ConversationSeq,
		&item.EventID, &payloadHash, &observationJSON, &item.State, &item.Attempt, &item.LeaseToken,
		&leaseUntil, &nextAttempt, &item.LastError, &createdAt, &updatedAt); err != nil {
		return domain.ContextOutboxItem{}, mapScanError(err)
	}
	if err := json.Unmarshal(observationJSON, &item.Observation); err != nil {
		return domain.ContextOutboxItem{}, err
	}
	if item.Observation.PayloadHash != payloadHash {
		return domain.ContextOutboxItem{}, errors.New("context outbox payload hash mismatch")
	}
	item.LeaseUntil, item.NextAttempt = fromUnixMillis(leaseUntil), fromUnixMillis(nextAttempt)
	item.CreatedAt, item.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	return item, nil
}
