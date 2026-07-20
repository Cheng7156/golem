package sqlite

import (
	"database/sql"

	"golem_plugin_hermes/internal/domain"
)

func scanMediaObject(row scanner) (domain.MediaObject, error) {
	var value domain.MediaObject
	var retainedUntil, createdAt, lastAccessAt int64
	if err := row.Scan(
		&value.ID, &value.Kind, &value.MIMEType, &value.Path, &value.Size, &value.SHA256,
		&retainedUntil, &createdAt, &lastAccessAt,
	); err != nil {
		return domain.MediaObject{}, mapScanError(err)
	}
	value.RetainedUntil = fromUnixMillis(retainedUntil)
	value.CreatedAt = fromUnixMillis(createdAt)
	value.LastAccessAt = fromUnixMillis(lastAccessAt)
	return value, nil
}

func scanMediaObjects(rows *sql.Rows) ([]domain.MediaObject, error) {
	var result []domain.MediaObject
	for rows.Next() {
		value, err := scanMediaObject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
