package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) CommitRunSuccess(
	ctx context.Context,
	runID string,
	leaseToken string,
	drafts []domain.OutboxDraft,
) ([]domain.OutboxItem, error) {
	return s.commitRunOutcome(ctx, runID, leaseToken, domain.RunSucceeded, domain.TurnCompleted, domain.InboxDone, "", nil, drafts)
}

func (s *Store) CommitRelayRunResult(ctx context.Context, runID, leaseToken string,
	proposal domain.RelayRunResult, drafts []domain.OutboxDraft) ([]domain.OutboxItem, error) {
	if err := proposal.Validate(); err != nil {
		return nil, err
	}
	if proposal.RunID != runID {
		return nil, storeport.ErrConflict
	}
	return s.commitRunOutcome(ctx, runID, leaseToken, domain.RunSucceeded, domain.TurnCompleted,
		domain.InboxDone, "", &proposal, drafts)
}

func (s *Store) CommitRunFailure(
	ctx context.Context,
	runID string,
	leaseToken string,
	message string,
	drafts []domain.OutboxDraft,
) ([]domain.OutboxItem, error) {
	return s.commitRunOutcome(ctx, runID, leaseToken, domain.RunFailed, domain.TurnFailed, domain.InboxFailed, message, nil, drafts)
}

func (s *Store) commitRunOutcome(
	ctx context.Context,
	runID string,
	leaseToken string,
	runTarget domain.RunState,
	turnTarget domain.TurnState,
	inboxTarget domain.InboxStatus,
	lastError string,
	proposal *domain.RelayRunResult,
	drafts []domain.OutboxDraft,
) ([]domain.OutboxItem, error) {
	success := runTarget == domain.RunSucceeded && turnTarget == domain.TurnCompleted && inboxTarget == domain.InboxDone
	failure := runTarget == domain.RunFailed && turnTarget == domain.TurnFailed && inboxTarget == domain.InboxFailed
	if !success && !failure {
		return nil, errors.New("unsupported run commit outcome")
	}
	var committed []domain.OutboxItem
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if proposal != nil {
			var existing domain.RelayRunResult
			var outboxJSON []byte
			var createdAt int64
			err := tx.QueryRowContext(ctx, `SELECT proposal_id,invocation_id,run_id,result_kind,result_hash,
				outbox_ids_json,created_at FROM relay_run_results WHERE proposal_id=?`, proposal.ProposalID).Scan(
				&existing.ProposalID, &existing.InvocationID, &existing.RunID, &existing.ResultKind,
				&existing.ResultHash, &outboxJSON, &createdAt)
			switch {
			case err == nil:
				if existing.InvocationID != proposal.InvocationID || existing.RunID != proposal.RunID ||
					existing.ResultKind != proposal.ResultKind || existing.ResultHash != proposal.ResultHash {
					return storeport.ErrConflict
				}
				return nil
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		run, err := scanRun(tx.QueryRowContext(ctx,
			`SELECT `+directRunColumns+` FROM runs WHERE id=?`,
			runID,
		))
		if err != nil {
			return err
		}
		if run.State != domain.RunRunning || run.LeaseToken == "" || run.LeaseToken != leaseToken {
			return storeport.ErrConflict
		}
		if err := domain.ValidateRunTransition(run.State, runTarget); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		if err := settleAmbientReplyBudget(ctx, tx, run, success && len(drafts) > 0, time.Now()); err != nil {
			return err
		}

		var turnState domain.TurnState
		if err := tx.QueryRowContext(ctx, `SELECT state FROM turns WHERE id=?`, run.TurnID).Scan(&turnState); err != nil {
			return mapScanError(err)
		}
		turnIntermediate := turnTarget
		if success {
			turnIntermediate = domain.TurnCommitting
		}
		if err := domain.ValidateTurnTransition(turnState, turnIntermediate); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		if success {
			if err := domain.ValidateTurnTransition(domain.TurnCommitting, turnTarget); err != nil {
				return errors.Join(storeport.ErrConflict, err)
			}
		}

		for _, draft := range drafts {
			if err := draft.Validate(); err != nil {
				return err
			}
			if draft.SessionID != run.SessionID {
				return errors.New("OutboxDraft 与 Run 的 session_id 不一致")
			}
		}

		now := time.Now()
		turnResult, err := tx.ExecContext(ctx,
			`UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?`,
			turnIntermediate,
			unixMillis(now),
			run.TurnID,
			turnState,
		)
		if err != nil {
			return err
		}
		if rows, err := turnResult.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return err
			}
			return storeport.ErrConflict
		}
		runResult, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET state=?,lease_token='',lease_until=0,next_attempt_at=0,last_error=?,updated_at=?
			WHERE id=? AND state=? AND lease_token=?
		`,
			runTarget,
			lastError,
			unixMillis(now),
			run.ID,
			domain.RunRunning,
			leaseToken,
		)
		if err != nil {
			return err
		}
		if rows, err := runResult.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return err
			}
			return storeport.ErrConflict
		}

		var nextSequence int64
		if len(drafts) > 0 {
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(sequence),0) FROM outbox WHERE session_id=?`,
				run.SessionID,
			).Scan(&nextSequence); err != nil {
				return err
			}
		}
		for _, draft := range drafts {
			nextSequence++
			id, err := newID("outbox")
			if err != nil {
				return err
			}
			item := domain.OutboxItem{
				ID:          id,
				RunID:       run.ID,
				SessionID:   draft.SessionID,
				ReceiverID:  draft.ReceiverID,
				Kind:        draft.Kind,
				Payload:     cloneBytes(draft.Payload),
				Sequence:    nextSequence,
				State:       domain.OutboxPending,
				NextAttempt: now,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO outbox(
					id,run_id,session_id,receiver_id,kind,payload_json,sequence,state,
					attempt,lease_token,lease_until,next_attempt_at,receipt_id,
					receipt_time,last_error,created_at,updated_at
				) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			`,
				item.ID,
				item.RunID,
				item.SessionID,
				item.ReceiverID,
				item.Kind,
				[]byte(item.Payload),
				item.Sequence,
				item.State,
				item.Attempt,
				item.LeaseToken,
				unixMillis(item.LeaseUntil),
				unixMillis(item.NextAttempt),
				item.ReceiptID,
				unixMillis(item.ReceiptTime),
				item.LastError,
				unixMillis(item.CreatedAt),
				unixMillis(item.UpdatedAt),
			); err != nil {
				return fmt.Errorf("写入 Transactional Outbox: %w", err)
			}
			committed = append(committed, item)
		}
		if success {
			result, err := tx.ExecContext(ctx,
				`UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?`,
				turnTarget,
				unixMillis(now),
				run.TurnID,
				domain.TurnCommitting,
			)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return storeport.ErrConflict
			}
		}
		if proposal != nil {
			outboxIDs := make([]string, 0, len(committed))
			for _, item := range committed {
				outboxIDs = append(outboxIDs, item.ID)
			}
			encoded, err := json.Marshal(outboxIDs)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO relay_run_results(
					proposal_id,invocation_id,run_id,result_kind,result_hash,outbox_ids_json,created_at
				) VALUES(?,?,?,?,?,?,?)`, proposal.ProposalID, proposal.InvocationID, run.ID,
				proposal.ResultKind, proposal.ResultHash, encoded, unixMillis(now)); err != nil {
				return fmt.Errorf("persist relay run result: %w", err)
			}
		}
		inboxResult, err := tx.ExecContext(ctx,
			`UPDATE inbox_events SET status=?,updated_at=? WHERE id=(SELECT event_id FROM turns WHERE id=?)`,
			inboxTarget,
			unixMillis(now),
			run.TurnID,
		)
		if err != nil {
			return err
		}
		if rows, err := inboxResult.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return err
			}
			return storeport.ErrConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
}

func (s *Store) GetRelayRunResult(ctx context.Context, proposalID string) (domain.RelayRunResult, error) {
	db, err := s.readable()
	if err != nil {
		return domain.RelayRunResult{}, err
	}
	var result domain.RelayRunResult
	var outboxJSON []byte
	var createdAt int64
	err = db.QueryRowContext(ctx, `SELECT proposal_id,invocation_id,run_id,result_kind,result_hash,
		outbox_ids_json,created_at FROM relay_run_results WHERE proposal_id=?`, proposalID).Scan(
		&result.ProposalID, &result.InvocationID, &result.RunID, &result.ResultKind, &result.ResultHash,
		&outboxJSON, &createdAt)
	if err != nil {
		return domain.RelayRunResult{}, mapScanError(err)
	}
	if err := json.Unmarshal(outboxJSON, &result.OutboxIDs); err != nil {
		return domain.RelayRunResult{}, err
	}
	result.CreatedAt = fromUnixMillis(createdAt)
	return result, nil
}

func (s *Store) GetRelayInvocationStatus(
	ctx context.Context,
	invocationID string,
) (domain.RelayInvocationStatus, error) {
	db, err := s.readable()
	if err != nil {
		return domain.RelayInvocationStatus{}, err
	}
	var result domain.RelayInvocationStatus
	var proposalID, resultKind, resultHash sql.NullString
	var outboxJSON []byte
	err = db.QueryRowContext(ctx, `
		SELECT r.invocation_id,r.id,r.state,r.last_error,
		       rr.proposal_id,rr.result_kind,rr.result_hash,rr.outbox_ids_json
		FROM runs r
		LEFT JOIN relay_run_results rr ON rr.run_id=r.id
		WHERE r.invocation_id=?
		ORDER BY r.created_at DESC
		LIMIT 1
	`, invocationID).Scan(
		&result.InvocationID, &result.RunID, &result.RunState, &result.LastError,
		&proposalID, &resultKind, &resultHash, &outboxJSON,
	)
	if err != nil {
		return domain.RelayInvocationStatus{}, mapScanError(err)
	}
	result.ProposalID = proposalID.String
	result.ResultKind = resultKind.String
	result.ResultHash = resultHash.String
	if len(outboxJSON) > 0 {
		if err := json.Unmarshal(outboxJSON, &result.OutboxIDs); err != nil {
			return domain.RelayInvocationStatus{}, err
		}
	}
	return result, nil
}
