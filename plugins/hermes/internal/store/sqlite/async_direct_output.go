package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) CommitAsyncDirectOutput(
	ctx context.Context,
	commit domain.AsyncDirectOutputCommit,
) (domain.AsyncDirectOutputResult, error) {
	if err := commit.Validate(); err != nil {
		return domain.AsyncDirectOutputResult{}, errors.Join(storeport.ErrInvalid, err)
	}
	var result domain.AsyncDirectOutputResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ticket, err := directOutputTicket(ctx, tx, commit)
		if err != nil {
			return err
		}
		input := directInsert{ticket: ticket, commit: commit}
		if existing, ok, err := existingDirectOutput(ctx, tx, input); err != nil {
			return err
		} else if ok {
			result = existing
			return nil
		}
		result, err = insertDirectOutput(ctx, tx, input)
		return err
	})
	return result, err
}

func (s *Store) CountAsyncDirectOutputs(ctx context.Context, ticketHash string) (int64, error) {
	db, err := s.readable()
	if err != nil {
		return 0, err
	}
	var count int64
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(d.id) FROM async_delivery_tickets t
		LEFT JOIN async_delivery_direct_outputs d ON d.ticket_id=t.id
		WHERE t.ticket_hash=? GROUP BY t.id
	`, ticketHash).Scan(&count)
	return count, mapScanError(err)
}

func directOutputTicket(
	ctx context.Context,
	tx *sql.Tx,
	commit domain.AsyncDirectOutputCommit,
) (domain.AsyncDeliveryTicket, error) {
	ticket, err := scanAsyncDelivery(tx.QueryRowContext(ctx, `
		SELECT `+asyncDeliveryColumns+` FROM async_delivery_tickets WHERE ticket_hash=?
	`, commit.TicketHash))
	if err != nil {
		return domain.AsyncDeliveryTicket{}, err
	}
	if ticket.State != domain.AsyncDeliveryPending || !directOutputMatches(ticket, commit) {
		return domain.AsyncDeliveryTicket{}, storeport.ErrConflict
	}
	if err := validateAsyncOutputPayload(commit.Output); err != nil {
		return domain.AsyncDeliveryTicket{}, err
	}
	return ticket, nil
}

func directOutputMatches(ticket domain.AsyncDeliveryTicket, commit domain.AsyncDirectOutputCommit) bool {
	return ticket.Profile == commit.Profile &&
		ticket.ProducerEpoch == commit.ProducerEpoch &&
		ticket.DelegationID == commit.DelegationID &&
		ticket.HermesSessionID == commit.HermesSessionID &&
		ticket.RelaySessionKey == commit.RelaySessionKey &&
		ticket.ChatID == commit.ChatID
}

func existingDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	input directInsert,
) (domain.AsyncDirectOutputResult, bool, error) {
	var result domain.AsyncDirectOutputResult
	var contentHash string
	err := tx.QueryRowContext(ctx, `
		SELECT d.outbox_id,d.content_hash,o.sequence FROM async_delivery_direct_outputs d
		JOIN outbox o ON o.id=d.outbox_id WHERE d.ticket_id=? AND d.invocation_id=?
	`, input.ticket.ID, input.commit.InvocationID).Scan(
		&result.OutboxID, &contentHash, &result.Sequence,
	)
	mapped := mapScanError(err)
	if errors.Is(mapped, storeport.ErrNotFound) {
		return result, false, nil
	}
	if mapped != nil {
		return result, false, mapped
	}
	if contentHash != directOutputHash(input.commit.Output) {
		return result, false, storeport.ErrConflict
	}
	count, err := countDirectOutputs(ctx, tx, input.ticket.ID)
	result.Queued = true
	result.DirectOutputCount = count
	return result, true, err
}

func insertDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	input directInsert,
) (domain.AsyncDirectOutputResult, error) {
	count, err := countDirectOutputs(ctx, tx, input.ticket.ID)
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	now := time.Now()
	input.count = count + 1
	input.now = now
	input.ids = directOutputIDs(input.ticket.ID)
	if count == 0 {
		payload, err := domain.AsyncDeliveryPayload(
			input.ticket, "", []domain.AsyncOutput{input.commit.Output},
		)
		if err != nil {
			return domain.AsyncDirectOutputResult{}, err
		}
		if err := insertAsyncAuditChain(ctx, tx, input.ticket, input.ids, payload, now); err != nil {
			return domain.AsyncDirectOutputResult{}, err
		}
	}
	return insertDirectOutbox(ctx, tx, input)
}

func insertDirectOutbox(
	ctx context.Context,
	tx *sql.Tx,
	input directInsert,
) (domain.AsyncDirectOutputResult, error) {
	sequence, err := nextOutboxSequence(ctx, tx, input.ticket.SessionID)
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	outboxID, err := newID("outbox")
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox(
			id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,attempt,
			lease_token,lease_until,next_attempt_at,receipt_id,receipt_time,last_error,
			created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, outboxID, input.ids.runID, input.ticket.SessionID, input.ticket.ReceiverID,
		input.commit.Output.Kind, []byte(input.commit.Output.Payload), sequence,
		domain.OutboxPending, 0, "", 0, unixMillis(input.now), 0, 0, "",
		unixMillis(input.now), unixMillis(input.now))
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	input.outboxID = outboxID
	input.sequence = sequence
	return recordDirectOutput(ctx, tx, input)
}

func recordDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	input directInsert,
) (domain.AsyncDirectOutputResult, error) {
	id, err := newID("ado")
	if err != nil {
		return domain.AsyncDirectOutputResult{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO async_delivery_direct_outputs(
			id,ticket_id,invocation_id,content_hash,outbox_id,created_at
		) VALUES(?,?,?,?,?,?)
	`, id, input.ticket.ID, input.commit.InvocationID,
		directOutputHash(input.commit.Output), input.outboxID,
		unixMillis(input.now))
	return domain.AsyncDirectOutputResult{
		Queued: err == nil, OutboxID: input.outboxID, Sequence: input.sequence,
		DirectOutputCount: input.count,
	}, err
}

type directInsert struct {
	ticket   domain.AsyncDeliveryTicket
	commit   domain.AsyncDirectOutputCommit
	ids      asyncIDs
	count    int64
	sequence int64
	outboxID string
	now      time.Time
}

func countDirectOutputs(ctx context.Context, tx *sql.Tx, ticketID string) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM async_delivery_direct_outputs WHERE ticket_id=?`,
		ticketID,
	).Scan(&count)
	return count, err
}

func nextOutboxSequence(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence),0)+1 FROM outbox WHERE session_id=?`,
		sessionID,
	).Scan(&sequence)
	return sequence, err
}

func directOutputIDs(ticketID string) asyncIDs {
	return asyncIDs{
		eventID:   "event_direct_" + ticketID,
		turnID:    "turn_direct_" + ticketID,
		runID:     "run_direct_" + ticketID,
		messageID: "direct_" + ticketID,
		dedupeKey: "async-direct:" + ticketID,
	}
}

func directOutputHash(output domain.AsyncOutput) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(output.Kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(output.Payload)
	return hex.EncodeToString(digest.Sum(nil))
}
