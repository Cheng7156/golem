package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenMigratesSchemaV1ToV11(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hermes.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := db.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("create V1 schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations(version,applied_at) VALUES(1,1)`,
	); err != nil {
		t.Fatalf("record V1 migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrate V1 database: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`,
	).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 11 {
		t.Fatalf("schema version=%d, want 11", version)
	}
	var table string
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='async_delivery_tickets'`,
	).Scan(&table); err != nil {
		t.Fatalf("async delivery table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='cron_delivery_bindings'`,
	).Scan(&table); err != nil {
		t.Fatalf("cron delivery table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='async_delivery_direct_outputs'`,
	).Scan(&table); err != nil {
		t.Fatalf("async direct output table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='media_objects'`,
	).Scan(&table); err != nil {
		t.Fatalf("media objects table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='cron_delivery_direct_outputs'`,
	).Scan(&table); err != nil {
		t.Fatalf("cron direct output table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='ambient_reply_budget'`,
	).Scan(&table); err != nil {
		t.Fatalf("ambient reply budget table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='async_video_jobs'`,
	).Scan(&table); err != nil {
		t.Fatalf("async video jobs table missing: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='async_video_urls'`,
	).Scan(&table); err != nil {
		t.Fatalf("async video URLs table missing: %v", err)
	}
	for _, name := range []string{"sticker_assets", "sticker_labels", "sticker_terms"} {
		if err := store.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name,
		).Scan(&table); err != nil {
			t.Fatalf("%s table missing: %v", name, err)
		}
	}
	var index string
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sticker_terms_lookup'`,
	).Scan(&index); err != nil {
		t.Fatalf("sticker term lookup index missing: %v", err)
	}
	var admissionColumn string
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM pragma_table_info('runs') WHERE name='admission_key'`,
	).Scan(&admissionColumn); err != nil {
		t.Fatalf("runs.admission_key missing after migration: %v", err)
	}
	var admissionIndex string
	if err := store.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_runs_admission'`,
	).Scan(&admissionIndex); err != nil {
		t.Fatalf("runs admission index missing after migration: %v", err)
	}
}
