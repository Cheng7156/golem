package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type admissionCandidate struct {
	id     string
	turnID string
	state  domain.RunState
}

// ReconcileRunAdmission keeps model execution bounded without dropping chat
// history. Every Inbox event already has a durable context_outbox observation;
// this method only supersedes redundant model Runs for that observation.
func (s *Store) ReconcileRunAdmission(
	ctx context.Context,
	runID string,
	mode domain.RunAdmissionMode,
) (domain.RunAdmissionResult, error) {
	result := domain.RunAdmissionResult{CurrentRunID: runID}
	if mode == domain.RunAdmissionOff {
		return result, nil
	}
	if mode != domain.RunAdmissionQueued && mode != domain.RunAdmissionActive {
		return result, storeport.ErrInvalid
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var sessionID string
		var trigger domain.TriggerKind
		var acceptSeq int64
		if err := tx.QueryRowContext(ctx, `
			SELECT r.session_id,r.trigger_kind,event.accept_seq
			FROM runs r
			JOIN turns t ON t.id=r.turn_id
			JOIN inbox_events event ON event.id=t.event_id
			WHERE r.id=?
		`, runID).Scan(&sessionID, &trigger, &acceptSeq); err != nil {
			return mapScanError(err)
		}

		var candidates []admissionCandidate
		var err error
		switch trigger {
		case domain.TriggerExplicit, domain.TriggerControl:
			candidates, err = admissionCandidates(ctx, tx, `
				SELECT r.id,r.turn_id,r.state
				FROM runs r
				JOIN turns t ON t.id=r.turn_id
				JOIN inbox_events event ON event.id=t.event_id
				WHERE r.session_id=? AND r.id<>? AND r.trigger_kind=?
				  AND event.accept_seq<?
				  AND r.state IN (?,?,?,?,?)
				ORDER BY event.accept_seq,r.created_at,r.id
			`, sessionID, runID, domain.TriggerAmbient, acceptSeq,
				domain.RunQueued, domain.RunLeased, domain.RunRunning,
				domain.RunRetryWait, domain.RunCancelRequested)
		case domain.TriggerAmbient:
			var newerForeground int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*)
				FROM runs newer
				JOIN turns t ON t.id=newer.turn_id
				JOIN inbox_events event ON event.id=t.event_id
				WHERE newer.session_id=? AND newer.id<>?
				  AND newer.trigger_kind IN (?,?) AND event.accept_seq>?
			`, sessionID, runID, domain.TriggerExplicit, domain.TriggerControl, acceptSeq).
				Scan(&newerForeground); err != nil {
				return err
			}
			if newerForeground > 0 {
				candidates, err = admissionCandidates(ctx, tx, `
					SELECT id,turn_id,state FROM runs WHERE id=? AND state IN (?,?,?,?,?)
				`, runID, domain.RunQueued, domain.RunLeased, domain.RunRunning,
					domain.RunRetryWait, domain.RunCancelRequested)
			} else {
				candidates, err = admissionCandidates(ctx, tx, `
					SELECT older.id,older.turn_id,older.state
					FROM runs older
					JOIN turns t ON t.id=older.turn_id
					JOIN inbox_events event ON event.id=t.event_id
					WHERE older.session_id=? AND older.id<>? AND older.trigger_kind=?
					  AND event.accept_seq<? AND older.state IN (?,?,?)
					ORDER BY event.accept_seq,older.created_at,older.id
				`, sessionID, runID, domain.TriggerAmbient, acceptSeq,
					domain.RunQueued, domain.RunLeased, domain.RunRetryWait)
			}
		default:
			return nil
		}
		if err != nil {
			return err
		}

		reason := "superseded by a newer ambient Run"
		if trigger == domain.TriggerExplicit || trigger == domain.TriggerControl {
			reason = "preempted by foreground Run " + runID
		} else if len(candidates) == 1 && candidates[0].id == runID {
			reason = "superseded by a newer foreground Run"
		}
		for _, candidate := range candidates {
			if mode != domain.RunAdmissionActive &&
				(candidate.state == domain.RunRunning || candidate.state == domain.RunCancelRequested) {
				continue
			}
			if candidate.id == runID {
				result.CurrentSuperseded = true
			}
			switch candidate.state {
			case domain.RunRunning:
				if err := domain.ValidateRunTransition(candidate.state, domain.RunCancelRequested); err != nil {
					return errors.Join(storeport.ErrConflict, err)
				}
				updated, err := tx.ExecContext(ctx, `
					UPDATE runs SET state=?,last_error=?,updated_at=? WHERE id=? AND state=?
				`, domain.RunCancelRequested, reason, unixMillis(time.Now()), candidate.id, candidate.state)
				if err != nil {
					return err
				}
				if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
					if err != nil {
						return err
					}
					return storeport.ErrConflict
				}
				result.CancelledRunIDs = append(result.CancelledRunIDs, candidate.id)
				result.InterruptRunIDs = append(result.InterruptRunIDs, candidate.id)
			case domain.RunCancelRequested:
				result.InterruptRunIDs = append(result.InterruptRunIDs, candidate.id)
			default:
				if err := cancelAdmissionRun(ctx, tx, candidate, reason); err != nil {
					return err
				}
				result.CancelledRunIDs = append(result.CancelledRunIDs, candidate.id)
			}
		}
		return nil
	})
	return result, err
}

func admissionCandidates(
	ctx context.Context,
	tx *sql.Tx,
	query string,
	args ...any,
) ([]admissionCandidate, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []admissionCandidate
	for rows.Next() {
		var candidate admissionCandidate
		if err := rows.Scan(&candidate.id, &candidate.turnID, &candidate.state); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func cancelAdmissionRun(
	ctx context.Context,
	tx *sql.Tx,
	candidate admissionCandidate,
	reason string,
) error {
	if err := domain.ValidateRunTransition(candidate.state, domain.RunCancelled); err != nil {
		return errors.Join(storeport.ErrConflict, err)
	}
	now := time.Now()
	updated, err := tx.ExecContext(ctx, `
		UPDATE runs SET state=?,lease_token='',lease_until=0,last_error=?,updated_at=?
		WHERE id=? AND state=?
	`, domain.RunCancelled, reason, unixMillis(now), candidate.id, candidate.state)
	if err != nil {
		return err
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return storeport.ErrConflict
	}
	return cancelTurn(ctx, tx, candidate.turnID, now)
}
