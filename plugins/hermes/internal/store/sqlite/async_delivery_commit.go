package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) CommitAsyncDelivery(
	ctx context.Context,
	commit domain.AsyncDeliveryCommit,
) (domain.AsyncDeliveryResult, error) {
	if err := commit.Validate(); err != nil {
		return domain.AsyncDeliveryResult{}, errors.Join(storeport.ErrInvalid, err)
	}
	var result domain.AsyncDeliveryResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ticket, err := scanAsyncDelivery(tx.QueryRowContext(ctx, `
			SELECT `+asyncDeliveryColumns+` FROM async_delivery_tickets WHERE ticket_hash=?
		`, commit.TicketHash))
		if err != nil {
			return err
		}
		if !asyncCommitMatches(ticket, commit) {
			return storeport.ErrConflict
		}
		if ticket.State != domain.AsyncDeliveryPending {
			result, err = terminalAsyncResult(ctx, tx, ticket)
			return err
		}
		return commitPendingAsyncDelivery(ctx, tx, ticket, commit, &result)
	})
	return result, err
}

func asyncCommitMatches(
	ticket domain.AsyncDeliveryTicket,
	commit domain.AsyncDeliveryCommit,
) bool {
	return ticket.Profile == commit.Profile &&
		ticket.ProducerEpoch == commit.ProducerEpoch &&
		ticket.DelegationID == commit.DelegationID &&
		ticket.HermesSessionID == commit.HermesSessionID &&
		ticket.RelaySessionKey == commit.RelaySessionKey &&
		ticket.ChatID == commit.ChatID
}

func terminalAsyncResult(
	ctx context.Context,
	tx *sql.Tx,
	ticket domain.AsyncDeliveryTicket,
) (domain.AsyncDeliveryResult, error) {
	disposition := "discarded"
	outboxIDs := []string(nil)
	if ticket.State == domain.AsyncDeliveryConsumed {
		disposition = "delivered"
		var err error
		outboxIDs, err = listAsyncOutboxIDs(ctx, tx, ticket)
		if err != nil {
			return domain.AsyncDeliveryResult{}, err
		}
		if len(outboxIDs) == 0 {
			disposition = "silent"
		}
	}
	return domain.AsyncDeliveryResult{
		State: ticket.State, Disposition: disposition,
		MessageID: ticket.ResultMessageID, OutboxID: ticket.OutboxID,
		OutboxIDs: outboxIDs,
	}, nil
}

func commitPendingAsyncDelivery(
	ctx context.Context,
	tx *sql.Tx,
	ticket domain.AsyncDeliveryTicket,
	commit domain.AsyncDeliveryCommit,
	result *domain.AsyncDeliveryResult,
) error {
	outputs, err := asyncOutputs(commit)
	if err != nil {
		return err
	}
	content := asyncAuditText(commit, outputs)
	payload, err := domain.AsyncDeliveryPayload(ticket, content, outputs)
	if err != nil {
		return err
	}
	now := time.Now()
	ids := asyncDeliveryIDs(ticket.ID)
	if err := insertAsyncAuditChain(ctx, tx, ticket, ids, payload, now); err != nil {
		return err
	}
	if commit.Silent {
		return consumeAsyncTicket(ctx, tx, ticket, ids.messageID, nil, now, result)
	}
	outboxIDs, err := insertAsyncOutboxes(ctx, tx, ticket, ids, outputs, now)
	if err != nil {
		return err
	}
	return consumeAsyncTicket(ctx, tx, ticket, ids.messageID, outboxIDs, now, result)
}

type asyncIDs struct {
	eventID   string
	turnID    string
	runID     string
	messageID string
	dedupeKey string
}

func asyncDeliveryIDs(ticketID string) asyncIDs {
	return asyncIDs{
		eventID:   "event_" + ticketID,
		turnID:    "turn_" + ticketID,
		runID:     "run_" + ticketID,
		messageID: "async_" + ticketID,
		dedupeKey: "async:" + ticketID,
	}
}

func insertAsyncAuditChain(
	ctx context.Context,
	tx *sql.Tx,
	ticket domain.AsyncDeliveryTicket,
	ids asyncIDs,
	payload json.RawMessage,
	now time.Time,
) error {
	binding, err := json.Marshal(ticket.Binding)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_events(
			id,dedupe_key,message_id,topic,session_id,occurred_at,accepted_at,
			binding_json,payload_json,status,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)
	`, ids.eventID, ids.dedupeKey, 0, "hermes.async_delivery",
		ticket.SessionID, unixMillis(now), unixMillis(now), binding, []byte(payload),
		domain.InboxDone, unixMillis(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turns(
			id,event_id,session_id,state,route,priority,base_session_version,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)
	`, ids.turnID, ids.eventID, ticket.SessionID, domain.TurnCompleted,
		domain.RouteAsyncDelivery, 0, 0, unixMillis(now), unixMillis(now)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs(
			id,turn_id,session_id,lane,state,revision,attempt,lease_token,lease_until,
			deadline,next_attempt_at,checkpoint_json,last_error,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, ids.runID, ids.turnID, ticket.SessionID, domain.LaneBackground,
		domain.RunSucceeded, 0, 1, "", 0, 0, 0, nil, "", unixMillis(now), unixMillis(now))
	return err
}

func insertAsyncOutboxes(
	ctx context.Context,
	tx *sql.Tx,
	ticket domain.AsyncDeliveryTicket,
	ids asyncIDs,
	outputs []domain.AsyncOutput,
	now time.Time,
) ([]string, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence),0)+1 FROM outbox WHERE session_id=?`,
		ticket.SessionID,
	).Scan(&sequence); err != nil {
		return nil, err
	}
	idsOut := make([]string, 0, len(outputs))
	for index, output := range outputs {
		if err := validateAsyncOutputPayload(output); err != nil {
			return nil, err
		}
		outboxID := asyncOutboxID(ticket.ID, index)
		if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox(
			id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,attempt,
			lease_token,lease_until,next_attempt_at,receipt_id,receipt_time,last_error,
			created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, outboxID, ids.runID, ticket.SessionID, ticket.ReceiverID, output.Kind, []byte(output.Payload),
			sequence, domain.OutboxPending, 0, "", 0, unixMillis(now), 0, 0, "",
			unixMillis(now), unixMillis(now)); err != nil {
			return nil, err
		}
		idsOut = append(idsOut, outboxID)
		sequence++
	}
	return idsOut, nil
}

func consumeAsyncTicket(
	ctx context.Context,
	tx *sql.Tx,
	ticket domain.AsyncDeliveryTicket,
	messageID string,
	outboxIDs []string,
	now time.Time,
	result *domain.AsyncDeliveryResult,
) error {
	outboxID := ""
	if len(outboxIDs) > 0 {
		outboxID = outboxIDs[0]
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE async_delivery_tickets
		SET state=?,result_message_id=?,outbox_id=?,updated_at=?
		WHERE id=? AND state=?
	`, domain.AsyncDeliveryConsumed, messageID, outboxID, unixMillis(now),
		ticket.ID, domain.AsyncDeliveryPending)
	if err != nil {
		return err
	}
	rows, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return storeport.ErrConflict
	}
	*result = domain.AsyncDeliveryResult{
		State: domain.AsyncDeliveryConsumed, Disposition: "delivered",
		MessageID: messageID, OutboxID: outboxID, OutboxIDs: outboxIDs,
	}
	if outboxID == "" {
		result.Disposition = "silent"
	}
	return nil
}

func listAsyncOutboxIDs(
	ctx context.Context,
	tx *sql.Tx,
	ticket domain.AsyncDeliveryTicket,
) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM outbox WHERE run_id=? ORDER BY sequence`,
		"run_"+ticket.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
