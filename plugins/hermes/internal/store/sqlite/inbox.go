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
