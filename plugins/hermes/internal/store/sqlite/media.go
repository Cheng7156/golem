package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const mediaObjectColumns = `
id,kind,mime_type,path,size,sha256,retained_until,created_at,last_access_at
`

func (s *Store) CreateMediaObject(
	ctx context.Context,
	value domain.MediaObject,
) (domain.MediaObject, bool, error) {
	if err := value.Validate(); err != nil {
		return domain.MediaObject{}, false, err
	}
	created := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO media_objects(
				id,kind,mime_type,path,size,sha256,retained_until,created_at,last_access_at
			) VALUES(?,?,?,?,?,?,?,?,?)
		`, value.ID, value.Kind, value.MIMEType, value.Path, value.Size, value.SHA256,
			unixMillis(value.RetainedUntil), unixMillis(value.CreatedAt), unixMillis(value.LastAccessAt))
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		created = err == nil && rows == 1
		return err
	})
	if err != nil {
		return domain.MediaObject{}, false, err
	}
	stored, err := s.GetMediaObject(ctx, value.ID)
	return stored, created, err
}

func (s *Store) GetMediaObject(ctx context.Context, id string) (domain.MediaObject, error) {
	db, err := s.readable()
	if err != nil {
		return domain.MediaObject{}, err
	}
	return scanMediaObject(db.QueryRowContext(ctx,
		`SELECT `+mediaObjectColumns+` FROM media_objects WHERE id=?`, id))
}

func (s *Store) RetainMediaObject(ctx context.Context, id string, until time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE media_objects
			SET retained_until=MAX(retained_until,?),last_access_at=?
			WHERE id=?
		`, unixMillis(until), unixMillis(time.Now()), id)
		if err != nil {
			return err
		}
		return requireMediaRow(result)
	})
}

func (s *Store) MediaStorageBytes(ctx context.Context) (int64, error) {
	db, err := s.readable()
	if err != nil {
		return 0, err
	}
	var total int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM media_objects`).Scan(&total)
	return total, err
}

func (s *Store) ListCollectibleMediaObjects(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]domain.MediaObject, error) {
	if limit <= 0 {
		return nil, errors.New("media collection limit must be positive")
	}
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT `+mediaObjectColumns+`
		FROM media_objects m
		WHERE m.retained_until<=?
		  AND NOT EXISTS (SELECT 1 FROM outbox_media_refs r WHERE r.object_id=m.id)
		ORDER BY m.last_access_at,m.created_at
		LIMIT ?
	`, unixMillis(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMediaObjects(rows)
}

func (s *Store) DeleteMediaObject(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM media_objects
			WHERE id=? AND NOT EXISTS(
				SELECT 1 FROM outbox_media_refs WHERE object_id=media_objects.id
			)
		`, id)
		if err != nil {
			return err
		}
		return requireMediaRow(result)
	})
}

func requireMediaRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return storeport.ErrConflict
	}
	return nil
}
