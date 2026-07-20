package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) RegisterCronDelivery(
	ctx context.Context,
	registration domain.CronDeliveryRegistration,
) (domain.CronDeliveryBinding, error) {
	if err := registration.Validate(); err != nil {
		return domain.CronDeliveryBinding{}, err
	}
	var binding domain.CronDeliveryBinding
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		existing, err := getCronBinding(ctx, tx, registration.Profile, registration.JobID)
		if err == nil {
			if existing.ChatID != registration.ChatID {
				return storeport.ErrConflict
			}
			binding = existing
			return nil
		}
		if !errors.Is(err, storeport.ErrNotFound) {
			return err
		}
		return insertCronBinding(ctx, tx, registration, &binding)
	})
	return binding, err
}

func insertCronBinding(
	ctx context.Context,
	tx *sql.Tx,
	registration domain.CronDeliveryRegistration,
	binding *domain.CronDeliveryBinding,
) error {
	channel, err := cronParentBinding(ctx, tx, registration.ParentRunID, registration.ChatID)
	if err != nil {
		return err
	}
	id, err := newID("cdb")
	if err != nil {
		return err
	}
	now := time.Now()
	value := domain.CronDeliveryBinding{
		ID: id, Profile: registration.Profile,
		JobID: registration.JobID, ChatID: registration.ChatID,
		SessionID: channel.SessionID, ReceiverID: channel.ReceiverID, Binding: channel,
		CreatedAt: now, UpdatedAt: now,
	}
	rawBinding, err := json.Marshal(channel)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO cron_delivery_bindings(
			id,profile,job_id,chat_id,session_id,receiver_id,
			binding_json,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)
	`, value.ID, value.Profile, value.JobID, value.ChatID,
		value.SessionID, value.ReceiverID, rawBinding, unixMillis(now), unixMillis(now))
	*binding = value
	return err
}

func cronParentBinding(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
	chatID string,
) (domain.ChannelBinding, error) {
	var state domain.RunState
	var bindingJSON []byte
	err := tx.QueryRowContext(ctx, `
		SELECT r.state,e.binding_json FROM runs r
		JOIN turns t ON t.id=r.turn_id
		JOIN inbox_events e ON e.id=t.event_id
		WHERE r.id=? AND (r.session_id || '|' || r.lane)=?
	`, runID, chatID).Scan(&state, &bindingJSON)
	if err != nil {
		return domain.ChannelBinding{}, mapScanError(err)
	}
	if state != domain.RunRunning {
		return domain.ChannelBinding{}, storeport.ErrConflict
	}
	var binding domain.ChannelBinding
	if err := json.Unmarshal(bindingJSON, &binding); err != nil {
		return domain.ChannelBinding{}, err
	}
	if err := binding.Validate(); err != nil {
		return domain.ChannelBinding{}, err
	}
	return binding, nil
}

func getCronBinding(
	ctx context.Context,
	tx *sql.Tx,
	profile string,
	jobID string,
) (domain.CronDeliveryBinding, error) {
	return scanCronBinding(tx.QueryRowContext(ctx, `
		SELECT id,profile,job_id,chat_id,session_id,receiver_id,
		       binding_json,created_at,updated_at
		FROM cron_delivery_bindings WHERE profile=? AND job_id=?
	`, profile, jobID))
}

func (s *Store) GetCronDelivery(
	ctx context.Context,
	profile string,
	jobID string,
) (domain.CronDeliveryBinding, error) {
	db, err := s.readable()
	if err != nil {
		return domain.CronDeliveryBinding{}, err
	}
	return scanCronBinding(db.QueryRowContext(ctx, `
		SELECT id,profile,job_id,chat_id,session_id,receiver_id,
		       binding_json,created_at,updated_at
		FROM cron_delivery_bindings WHERE profile=? AND job_id=?
	`, strings.TrimSpace(profile), strings.TrimSpace(jobID)))
}

func scanCronBinding(scanner interface{ Scan(...any) error }) (domain.CronDeliveryBinding, error) {
	var value domain.CronDeliveryBinding
	var rawBinding []byte
	var createdAt, updatedAt int64
	err := scanner.Scan(
		&value.ID, &value.Profile, &value.JobID, &value.ChatID,
		&value.SessionID, &value.ReceiverID, &rawBinding, &createdAt, &updatedAt,
	)
	if err != nil {
		return domain.CronDeliveryBinding{}, mapScanError(err)
	}
	if err := json.Unmarshal(rawBinding, &value.Binding); err != nil {
		return domain.CronDeliveryBinding{}, err
	}
	value.CreatedAt = fromUnixMillis(createdAt)
	value.UpdatedAt = fromUnixMillis(updatedAt)
	return value, nil
}

func cronContentHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}
