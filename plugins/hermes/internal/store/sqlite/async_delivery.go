package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const asyncDeliveryColumns = `
id,ticket_hash,profile,producer_epoch,delegation_id,hermes_session_id,
relay_session_key,chat_id,session_id,receiver_id,binding_json,parent_run_id,
state,result_message_id,outbox_id,last_error,created_at,updated_at`

func (s *Store) RegisterAsyncDelivery(
	ctx context.Context,
	registration domain.AsyncDeliveryRegistration,
) (domain.AsyncDeliveryTicket, error) {
	if err := registration.Validate(); err != nil {
		return domain.AsyncDeliveryTicket{}, err
	}
	var ticket domain.AsyncDeliveryTicket
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		existing, err := getAsyncDeliveryByDelegation(ctx, tx, registration)
		if err == nil {
			ticket = existing
			return nil
		}
		if !errors.Is(err, storeport.ErrNotFound) {
			return err
		}
		if _, err := scanAsyncDelivery(tx.QueryRowContext(ctx, `
			SELECT `+asyncDeliveryColumns+` FROM async_delivery_tickets WHERE ticket_hash=?
		`, registration.TicketHash)); err == nil {
			return storeport.ErrConflict
		} else if !errors.Is(err, storeport.ErrNotFound) {
			return err
		}
		if err := abandonPreviousEpochs(ctx, tx, registration); err != nil {
			return err
		}
		return insertAsyncDelivery(ctx, tx, registration, &ticket)
	})
	return ticket, err
}

func getAsyncDeliveryByDelegation(
	ctx context.Context,
	tx *sql.Tx,
	registration domain.AsyncDeliveryRegistration,
) (domain.AsyncDeliveryTicket, error) {
	value, err := scanAsyncDelivery(tx.QueryRowContext(ctx, `
		SELECT `+asyncDeliveryColumns+` FROM async_delivery_tickets
		WHERE profile=? AND producer_epoch=? AND delegation_id=?
	`, registration.Profile, registration.ProducerEpoch, registration.DelegationID))
	if err != nil {
		return domain.AsyncDeliveryTicket{}, err
	}
	if value.TicketHash != registration.TicketHash ||
		value.HermesSessionID != registration.HermesSessionID ||
		value.RelaySessionKey != registration.RelaySessionKey ||
		value.ChatID != registration.ChatID ||
		value.ParentRunID != registration.ParentRunID {
		return domain.AsyncDeliveryTicket{}, storeport.ErrConflict
	}
	return value, nil
}

func abandonPreviousEpochs(
	ctx context.Context,
	tx *sql.Tx,
	registration domain.AsyncDeliveryRegistration,
) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE async_delivery_tickets SET state=?,last_error=?,updated_at=?
		WHERE profile=? AND producer_epoch<>? AND state=?
	`, domain.AsyncDeliveryAbandoned, "superseded producer epoch",
		unixMillis(time.Now()), registration.Profile, registration.ProducerEpoch,
		domain.AsyncDeliveryPending)
	return err
}

func insertAsyncDelivery(
	ctx context.Context,
	tx *sql.Tx,
	registration domain.AsyncDeliveryRegistration,
	ticket *domain.AsyncDeliveryTicket,
) error {
	binding, sessionID, receiverID, err := parentAsyncBinding(ctx, tx, registration.ParentRunID)
	if err != nil {
		return err
	}
	id, err := newID("adt")
	if err != nil {
		return err
	}
	now := time.Now()
	value := domain.AsyncDeliveryTicket{
		ID: id, TicketHash: registration.TicketHash, Profile: registration.Profile,
		ProducerEpoch: registration.ProducerEpoch, DelegationID: registration.DelegationID,
		HermesSessionID: registration.HermesSessionID,
		RelaySessionKey: registration.RelaySessionKey, ChatID: registration.ChatID,
		SessionID: sessionID, ReceiverID: receiverID, Binding: binding,
		ParentRunID: registration.ParentRunID, State: domain.AsyncDeliveryPending,
		CreatedAt: now, UpdatedAt: now,
	}
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO async_delivery_tickets(`+asyncDeliveryColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, value.ID, value.TicketHash, value.Profile, value.ProducerEpoch,
		value.DelegationID, value.HermesSessionID, value.RelaySessionKey,
		value.ChatID, value.SessionID, value.ReceiverID, bindingJSON,
		value.ParentRunID, value.State, "", "", "", unixMillis(now), unixMillis(now))
	*ticket = value
	return err
}

func parentAsyncBinding(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
) (domain.ChannelBinding, string, string, error) {
	var state domain.RunState
	var sessionID, bindingJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT r.state,r.session_id,e.binding_json
		FROM runs r JOIN turns t ON t.id=r.turn_id
		JOIN inbox_events e ON e.id=t.event_id WHERE r.id=?
	`, runID).Scan(&state, &sessionID, &bindingJSON)
	if err != nil {
		return domain.ChannelBinding{}, "", "", mapScanError(err)
	}
	if state != domain.RunRunning {
		return domain.ChannelBinding{}, "", "", storeport.ErrConflict
	}
	var binding domain.ChannelBinding
	if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
		return domain.ChannelBinding{}, "", "", err
	}
	if err := binding.Validate(); err != nil || binding.SessionID != sessionID {
		return domain.ChannelBinding{}, "", "", errors.Join(storeport.ErrConflict, err)
	}
	return binding, sessionID, binding.ReceiverID, nil
}

func (s *Store) GetAsyncDelivery(
	ctx context.Context,
	ticketHash string,
) (domain.AsyncDeliveryTicket, error) {
	db, err := s.readable()
	if err != nil {
		return domain.AsyncDeliveryTicket{}, err
	}
	return scanAsyncDelivery(db.QueryRowContext(ctx, `
		SELECT `+asyncDeliveryColumns+` FROM async_delivery_tickets WHERE ticket_hash=?
	`, strings.TrimSpace(ticketHash)))
}

func scanAsyncDelivery(scanner interface{ Scan(...any) error }) (domain.AsyncDeliveryTicket, error) {
	var value domain.AsyncDeliveryTicket
	var bindingJSON []byte
	var createdAt, updatedAt int64
	err := scanner.Scan(
		&value.ID, &value.TicketHash, &value.Profile, &value.ProducerEpoch,
		&value.DelegationID, &value.HermesSessionID, &value.RelaySessionKey,
		&value.ChatID, &value.SessionID, &value.ReceiverID, &bindingJSON,
		&value.ParentRunID, &value.State, &value.ResultMessageID, &value.OutboxID,
		&value.LastError, &createdAt, &updatedAt,
	)
	if err != nil {
		return domain.AsyncDeliveryTicket{}, mapScanError(err)
	}
	if err := json.Unmarshal(bindingJSON, &value.Binding); err != nil {
		return domain.AsyncDeliveryTicket{}, fmt.Errorf("decode async delivery binding: %w", err)
	}
	value.CreatedAt = fromUnixMillis(createdAt)
	value.UpdatedAt = fromUnixMillis(updatedAt)
	return value, nil
}
