package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) AcceptInbox(ctx context.Context, event domain.InboxEvent) (domain.InboxEvent, bool, error) {
	if event.Status == "" {
		event.Status = domain.InboxAccepted
	}
	if event.Status != domain.InboxAccepted {
		return domain.InboxEvent{}, false, fmt.Errorf("新 Inbox Event 必须为 accepted，实际为 %s", event.Status)
	}
	if event.AcceptedAt.IsZero() {
		event.AcceptedAt = time.Now()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = event.AcceptedAt
	}
	if err := event.Validate(); err != nil {
		return domain.InboxEvent{}, false, err
	}
	binding, err := json.Marshal(event.Binding)
	if err != nil {
		return domain.InboxEvent{}, false, err
	}

	var stored domain.InboxEvent
	inserted := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO inbox_events(
				id,dedupe_key,message_id,topic,session_id,occurred_at,accepted_at,
				binding_json,payload_json,status,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		`,
			event.ID,
			event.DedupeKey,
			event.MessageID,
			event.Topic,
			event.SessionID,
			unixMillis(event.OccurredAt),
			unixMillis(event.AcceptedAt),
			binding,
			[]byte(event.Payload),
			event.Status,
			unixMillis(event.AcceptedAt),
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		inserted = rows == 1
		query := `SELECT ` + inboxColumns + ` FROM inbox_events WHERE dedupe_key=? OR id=? ORDER BY accept_seq LIMIT 1`
		stored, err = scanInbox(tx.QueryRowContext(ctx, query, event.DedupeKey, event.ID))
		if err != nil {
			return err
		}
		var contextExists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_outbox WHERE event_id=?`, stored.ID).Scan(&contextExists); err != nil {
			return err
		}
		if contextExists != 0 {
			return nil
		}
		observation, err := domain.NewConversationObservation(stored)
		if err != nil {
			return fmt.Errorf("build conversation observation: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(conversation_seq),0)+1 FROM context_outbox WHERE conversation_id=?`,
			observation.ConversationID,
		).Scan(&observation.ConversationSeq); err != nil {
			return err
		}
		if err := domain.FinalizeObservationHash(&observation); err != nil {
			return err
		}
		observationJSON, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO context_outbox(
				id,conversation_id,accept_seq,conversation_seq,event_id,payload_hash,observation_json,state,attempt,
				lease_token,lease_until,next_attempt_at,last_error,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, observation.ObservationID, observation.ConversationID, observation.AcceptSeq, observation.ConversationSeq,
			stored.ID, observation.PayloadHash, observationJSON, domain.ContextPending, 0, "", 0,
			unixMillis(stored.AcceptedAt), "", unixMillis(stored.AcceptedAt), unixMillis(stored.AcceptedAt))
		return err
	})
	if err != nil {
		return domain.InboxEvent{}, false, err
	}
	return stored, inserted, nil
}

func (s *Store) ListInbox(ctx context.Context, status domain.InboxStatus, limit int) ([]domain.InboxEvent, error) {
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+inboxColumns+` FROM inbox_events WHERE status=? ORDER BY accept_seq LIMIT ?`,
		status,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []domain.InboxEvent
	for rows.Next() {
		value, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) GetInbox(ctx context.Context, id string) (domain.InboxEvent, error) {
	db, err := s.readable()
	if err != nil {
		return domain.InboxEvent{}, err
	}
	return scanInbox(db.QueryRowContext(ctx,
		`SELECT `+inboxColumns+` FROM inbox_events WHERE id=?`,
		id,
	))
}

func (s *Store) TransitionInbox(ctx context.Context, id string, from, to domain.InboxStatus) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE inbox_events SET status=?,updated_at=? WHERE id=? AND status=?`,
			to,
			unixMillis(time.Now()),
			id,
			from,
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
