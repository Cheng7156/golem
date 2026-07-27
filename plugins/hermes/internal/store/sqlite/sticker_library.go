package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const stickerAssetColumns = `
id,mime_type,path,size,sha256,created_at
`

func (s *Store) StoreStickerCollection(
	ctx context.Context,
	asset domain.StickerAsset,
	label domain.StickerLabel,
	terms []domain.StickerSearchTerm,
	maxStorageBytes int64,
) (domain.StickerCollectionResult, error) {
	if err := asset.Validate(); err != nil {
		return domain.StickerCollectionResult{}, err
	}
	if err := label.Validate(); err != nil || label.StickerID != asset.ID {
		if err != nil {
			return domain.StickerCollectionResult{}, err
		}
		return domain.StickerCollectionResult{}, errors.New("sticker label does not match asset")
	}
	terms = validStickerTerms(terms)
	if len(terms) == 0 {
		return domain.StickerCollectionResult{}, errors.New("sticker collection requires search terms")
	}
	if maxStorageBytes <= 0 {
		return domain.StickerCollectionResult{}, errors.New("sticker collection requires a storage budget")
	}
	result := domain.StickerCollectionResult{Asset: asset}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var existing int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sticker_assets WHERE id=?`, asset.ID,
		).Scan(&existing); err != nil {
			return err
		}
		if existing == 0 {
			var total int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(size),0) FROM sticker_assets`,
			).Scan(&total); err != nil {
				return err
			}
			if total+asset.Size > maxStorageBytes {
				return storeport.ErrCapacity
			}
		}
		insert, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO sticker_assets(
				id,mime_type,path,size,sha256,created_at
			) VALUES(?,?,?,?,?,?)
		`, asset.ID, asset.MIMEType, asset.Path, asset.Size, asset.SHA256,
			unixMillis(asset.CreatedAt))
		if err != nil {
			return err
		}
		rows, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		result.AssetCreated = rows == 1

		stored, err := scanStickerAsset(tx.QueryRowContext(ctx,
			`SELECT `+stickerAssetColumns+` FROM sticker_assets WHERE id=?`, asset.ID))
		if err != nil {
			return err
		}
		if stored.MIMEType != asset.MIMEType || stored.Path != asset.Path ||
			stored.Size != asset.Size || stored.SHA256 != asset.SHA256 {
			return errors.New("sticker asset digest collision")
		}
		result.Asset = stored

		insert, err = tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO sticker_labels(
				sticker_id,description,description_norm,source_session_id,
				source_event_id,source_message_id,source_speaker_id,
				source_speaker_name,collected_by_id,collected_by_name,created_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		`, label.StickerID, label.Description, label.DescriptionNorm,
			label.SourceSessionID, label.SourceEventID, label.SourceMessageID,
			label.SourceSpeakerID, label.SourceSpeakerName, label.CollectedByID,
			label.CollectedByName, unixMillis(label.CreatedAt))
		if err != nil {
			return err
		}
		rows, err = insert.RowsAffected()
		if err != nil {
			return err
		}
		result.LabelCreated = rows == 1
		for _, term := range terms {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO sticker_terms(sticker_id,term,weight)
				VALUES(?,?,?)
				ON CONFLICT(sticker_id,term) DO UPDATE SET weight=MAX(weight,excluded.weight)
			`, asset.ID, term.Value, term.Weight); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func validStickerTerms(values []domain.StickerSearchTerm) []domain.StickerSearchTerm {
	result := make([]domain.StickerSearchTerm, 0, len(values))
	seen := make(map[string]int, len(values))
	for _, value := range values {
		value.Value = strings.TrimSpace(value.Value)
		if value.Value == "" || value.Weight <= 0 || len([]byte(value.Value)) > 256 {
			continue
		}
		if index, exists := seen[value.Value]; exists {
			if value.Weight > result[index].Weight {
				result[index].Weight = value.Weight
			}
			continue
		}
		seen[value.Value] = len(result)
		result = append(result, value)
	}
	return result
}

func (s *Store) GetStickerAsset(ctx context.Context, id string) (domain.StickerAsset, error) {
	db, err := s.readable()
	if err != nil {
		return domain.StickerAsset{}, err
	}
	return scanStickerAsset(db.QueryRowContext(ctx,
		`SELECT `+stickerAssetColumns+` FROM sticker_assets WHERE id=?`, strings.TrimSpace(id)))
}

func (s *Store) StickerLibraryStorageBytes(ctx context.Context) (int64, error) {
	db, err := s.readable()
	if err != nil {
		return 0, err
	}
	var total int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM sticker_assets`).Scan(&total)
	return total, err
}

func (s *Store) SearchStickerLibrary(
	ctx context.Context,
	terms []domain.StickerSearchTerm,
	limit int,
) ([]domain.StickerLibraryMatch, error) {
	terms = validStickerTerms(terms)
	if len(terms) == 0 || limit < 1 || limit > 100 {
		return nil, storeport.ErrInvalid
	}
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	placeholders := make([]string, 0, len(terms))
	arguments := make([]any, 0, len(terms)*2+1)
	for _, term := range terms {
		placeholders = append(placeholders, "(?,?)")
		arguments = append(arguments, term.Value, term.Weight)
	}
	arguments = append(arguments, limit)
	query := `WITH query_terms(term,weight) AS (VALUES ` + strings.Join(placeholders, ",") + `)
		SELECT a.id,
			COALESCE((SELECT group_concat(description,'；') FROM (
				SELECT description FROM sticker_labels
				WHERE sticker_id=a.id ORDER BY created_at DESC LIMIT 8
			)),''),
			SUM(CASE WHEN t.weight<q.weight THEN t.weight ELSE q.weight END) AS score
		FROM query_terms q
		JOIN sticker_terms t ON t.term=q.term
		JOIN sticker_assets a ON a.id=t.sticker_id
		GROUP BY a.id
		ORDER BY score DESC,a.created_at DESC
		LIMIT ?`
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.StickerLibraryMatch
	for rows.Next() {
		var value domain.StickerLibraryMatch
		if err := rows.Scan(&value.StickerID, &value.Description, &value.Score); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) StickerLibraryInventory(
	ctx context.Context,
	limit int,
	offset int,
) ([]domain.StickerLibraryInventoryItem, int, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, storeport.ErrInvalid
	}
	db, err := s.readable()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sticker_assets`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.id,
			COALESCE((SELECT group_concat(description,'；') FROM (
				SELECT description FROM sticker_labels
				WHERE sticker_id=a.id ORDER BY created_at DESC LIMIT 8
			)),'') AS description,
			a.created_at
		FROM sticker_assets a
		ORDER BY a.created_at DESC,a.id
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]domain.StickerLibraryInventoryItem, 0, min(limit, total))
	for rows.Next() {
		var value domain.StickerLibraryInventoryItem
		var createdAt int64
		if err := rows.Scan(&value.StickerID, &value.Description, &createdAt); err != nil {
			return nil, 0, err
		}
		value.CreatedAt = fromUnixMillis(createdAt)
		result = append(result, value)
	}
	return result, total, rows.Err()
}

func (s *Store) FindRecentSentStickerAsset(
	ctx context.Context,
	sessionID string,
) (domain.StickerAsset, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return domain.StickerAsset{}, storeport.ErrInvalid
	}
	db, err := s.readable()
	if err != nil {
		return domain.StickerAsset{}, err
	}
	var payload []byte
	if err := db.QueryRowContext(ctx, `
		SELECT payload_json FROM outbox
		WHERE session_id=? AND kind='emoji' AND state=?
		ORDER BY receipt_time DESC,sequence DESC
		LIMIT 1
	`, sessionID, domain.OutboxSent).Scan(&payload); err != nil {
		return domain.StickerAsset{}, mapScanError(err)
	}
	var output domain.EmojiOutput
	if err := json.Unmarshal(payload, &output); err != nil || len(output.Data) == 0 {
		return domain.StickerAsset{}, storeport.ErrNotFound
	}
	digestBytes := sha256.Sum256(output.Data)
	digest := hex.EncodeToString(digestBytes[:])
	return scanStickerAsset(db.QueryRowContext(ctx,
		`SELECT `+stickerAssetColumns+` FROM sticker_assets WHERE sha256=?`, digest))
}

func (s *Store) ReplaceStickerDescription(
	ctx context.Context,
	label domain.StickerLabel,
	terms []domain.StickerSearchTerm,
) error {
	if err := label.Validate(); err != nil {
		return errors.Join(storeport.ErrInvalid, err)
	}
	terms = validStickerTerms(terms)
	if len(terms) == 0 {
		return storeport.ErrInvalid
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sticker_assets WHERE id=?`, label.StickerID,
		).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return storeport.ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sticker_labels WHERE sticker_id=?`, label.StickerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sticker_terms WHERE sticker_id=?`, label.StickerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sticker_labels(
				sticker_id,description,description_norm,source_session_id,
				source_event_id,source_message_id,source_speaker_id,
				source_speaker_name,collected_by_id,collected_by_name,created_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		`, label.StickerID, label.Description, label.DescriptionNorm,
			label.SourceSessionID, label.SourceEventID, label.SourceMessageID,
			label.SourceSpeakerID, label.SourceSpeakerName, label.CollectedByID,
			label.CollectedByName, unixMillis(label.CreatedAt)); err != nil {
			return err
		}
		for _, term := range terms {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO sticker_terms(sticker_id,term,weight) VALUES(?,?,?)`,
				label.StickerID, term.Value, term.Weight,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteStickerAsset(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return storeport.ErrInvalid
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM sticker_assets WHERE id=?`, id)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return storeport.ErrNotFound
		}
		return nil
	})
}

func scanStickerAsset(row scanner) (domain.StickerAsset, error) {
	var value domain.StickerAsset
	var createdAt int64
	if err := row.Scan(
		&value.ID, &value.MIMEType, &value.Path, &value.Size, &value.SHA256,
		&createdAt,
	); err != nil {
		return domain.StickerAsset{}, mapScanError(err)
	}
	value.CreatedAt = fromUnixMillis(createdAt)
	return value, nil
}
