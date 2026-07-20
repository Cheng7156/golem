package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(6,?)`,
			unixMillis(time.Now()),
		)
		return err
	})
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
