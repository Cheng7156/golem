package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"

	_ "modernc.org/sqlite"
)

type Store struct {
	db        *sql.DB
	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error
}

var _ storeport.Store = (*Store)(nil)

func Open(ctx context.Context, path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("SQLite 路径不能为空")
	}
	if path != ":memory:" {
		path = filepath.Clean(path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("创建 SQLite 目录: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file:hermes-memory?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite: %w", err)
	}
	value := &Store{db: db}
	if err := value.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return value, nil
}

func (s *Store) migrate(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("执行 Hermes Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
			return fmt.Errorf("执行 Hermes Async Delivery Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
			return fmt.Errorf("执行 Hermes Cron Delivery Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("execute Hermes Direct Delivery Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV5); err != nil {
			return fmt.Errorf("execute Hermes Media Object Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV6); err != nil {
			return fmt.Errorf("execute Hermes Cron Direct Output Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV7); err != nil {
			return fmt.Errorf("execute Hermes Observation V2 Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
			return fmt.Errorf("execute Hermes Ambient Reply Budget Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV9); err != nil {
			return fmt.Errorf("execute Hermes Async Video Job Schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV10); err != nil {
			return fmt.Errorf("execute Hermes Outbox Delivery State Schema: %w", err)
		}
		if err := ensureTableColumn(
			ctx, tx, "async_video_jobs", "stage", "TEXT NOT NULL DEFAULT 'queued'",
		); err != nil {
			return err
		}
		if err := ensureTableColumn(
			ctx, tx, "async_video_jobs", "source_url", "TEXT NOT NULL DEFAULT ''",
		); err != nil {
			return err
		}
		if err := ensureTableColumn(
			ctx, tx, "async_video_jobs", "auto_close", "INTEGER NOT NULL DEFAULT 0",
		); err != nil {
			return err
		}
		for _, column := range []struct{ name, ddl string }{
			{"conversation_id", "TEXT NOT NULL DEFAULT ''"},
			{"current_observation_id", "TEXT NOT NULL DEFAULT ''"},
			{"current_payload_hash", "TEXT NOT NULL DEFAULT ''"},
			{"required_context_seq", "INTEGER NOT NULL DEFAULT 0"},
			{"trigger_kind", "TEXT NOT NULL DEFAULT 'ambient'"},
			{"invocation_id", "TEXT NOT NULL DEFAULT ''"},
			{"admission_key", "TEXT NOT NULL DEFAULT ''"},
		} {
			if err := ensureTableColumn(ctx, tx, "runs", column.name, column.ddl); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`CREATE INDEX IF NOT EXISTS idx_runs_invocation ON runs(invocation_id)`,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`CREATE INDEX IF NOT EXISTS idx_runs_admission
			 ON runs(session_id,lane,admission_key,state,next_attempt_at,created_at)`,
		); err != nil {
			return err
		}
		// Older databases predate the per-sender admission key.  Recover it from
		// the connector-authenticated Inbox binding without changing the public
		// session id or any conversation history.
		if _, err := tx.ExecContext(ctx, `UPDATE runs
			SET admission_key=COALESCE(NULLIF(admission_key,''),
				CASE WHEN lane=? AND (json_extract((SELECT e.payload_json
					FROM turns t JOIN inbox_events e ON e.id=t.event_id
					WHERE t.id=runs.turn_id),'$.is_chatroom')=1 OR session_id LIKE 'chatroom:%')
					THEN COALESCE(json_extract((SELECT e.binding_json
						FROM turns t JOIN inbox_events e ON e.id=t.event_id
						WHERE t.id=runs.turn_id),'$.principal.id'),'')
					ELSE '' END)
			WHERE admission_key=''`, domain.LaneInteractive); err != nil {
			return err
		}
		if err := backfillPendingContext(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(1,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(2,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(3,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(4,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(5,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(6,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(7,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(8,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(9,?)`,
			unixMillis(time.Now()),
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(10,?)`,
			unixMillis(time.Now()),
		)
		return err
	})
}

func backfillPendingContext(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM inbox_events
		WHERE status IN (?,?,?) ORDER BY accept_seq`, domain.InboxAccepted, domain.InboxOrdered, domain.InboxRouted)
	if err != nil {
		return err
	}
	var events []domain.InboxEvent
	for rows.Next() {
		event, scanErr := scanInbox(rows)
		if scanErr != nil {
			_ = rows.Close()
			return scanErr
		}
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, event := range events {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_outbox WHERE event_id=?`, event.ID).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		observation, err := domain.NewConversationObservation(event)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(conversation_seq),0)+1 FROM context_outbox WHERE conversation_id=?`,
			observation.ConversationID).Scan(&observation.ConversationSeq); err != nil {
			return err
		}
		if err := domain.FinalizeObservationHash(&observation); err != nil {
			return err
		}
		payload, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO context_outbox(
			id,conversation_id,accept_seq,conversation_seq,event_id,payload_hash,observation_json,
			state,attempt,lease_token,lease_until,next_attempt_at,last_error,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, observation.ObservationID, observation.ConversationID,
			observation.AcceptSeq, observation.ConversationSeq, event.ID, observation.PayloadHash, payload,
			domain.ContextPending, 0, "", 0, unixMillis(event.AcceptedAt), "",
			unixMillis(event.AcceptedAt), unixMillis(event.AcceptedAt)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE runs SET
		conversation_id=COALESCE(NULLIF(conversation_id,''),(SELECT c.conversation_id FROM turns t JOIN context_outbox c ON c.event_id=t.event_id WHERE t.id=runs.turn_id)),
		current_observation_id=COALESCE(NULLIF(current_observation_id,''),(SELECT c.id FROM turns t JOIN context_outbox c ON c.event_id=t.event_id WHERE t.id=runs.turn_id)),
		current_payload_hash=COALESCE(NULLIF(current_payload_hash,''),(SELECT c.payload_hash FROM turns t JOIN context_outbox c ON c.event_id=t.event_id WHERE t.id=runs.turn_id)),
		required_context_seq=CASE WHEN required_context_seq=0 THEN COALESCE((SELECT c.conversation_seq FROM turns t JOIN context_outbox c ON c.event_id=t.event_id WHERE t.id=runs.turn_id),0) ELSE required_context_seq END,
		invocation_id=CASE WHEN invocation_id='' AND EXISTS(SELECT 1 FROM turns t JOIN context_outbox c ON c.event_id=t.event_id WHERE t.id=runs.turn_id) THEN 'invoke_v1:'||id||':'||revision ELSE invocation_id END
		WHERE state IN ('queued','retry_wait','leased','running','cancel_requested')`)
	return err
}

func ensureTableColumn(ctx context.Context, tx *sql.Tx, table, name, ddl string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+name+` `+ddl)
	return err
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.closed.Load() {
		return storeport.ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return storeport.ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) readable() (*sql.DB, error) {
	if s.closed.Load() {
		return nil, storeport.ErrClosed
	}
	return s.db, nil
}

func newID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(data[:]), nil
}

func unixMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

func fromUnixMillis(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
