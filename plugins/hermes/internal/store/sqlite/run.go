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

func (s *Store) CreateRun(ctx context.Context, value domain.Run) (domain.Run, error) {
	if strings.TrimSpace(value.TurnID) == "" || strings.TrimSpace(value.SessionID) == "" {
		return domain.Run{}, errors.New("Run 缺少 turn_id 或 session_id")
	}
	switch value.Lane {
	case domain.LaneControl, domain.LaneInteractive, domain.LaneJob, domain.LaneBackground:
	default:
		return domain.Run{}, fmt.Errorf("不支持的 Run Lane: %s", value.Lane)
	}
	if value.ID == "" {
		id, err := newID("run")
		if err != nil {
			return domain.Run{}, err
		}
		value.ID = id
	}
	if value.State == "" {
		value.State = domain.RunQueued
	}
	if value.State != domain.RunQueued {
		return domain.Run{}, fmt.Errorf("新 Run 必须为 queued，实际为 %s", value.State)
	}
	if value.Revision < 0 {
		return domain.Run{}, errors.New("Run revision 不能为负数")
	}
	if value.ConversationID != "" && (value.CurrentObservationID == "" || value.RequiredContextSeq <= 0) {
		return domain.Run{}, errors.New("Run observation context is incomplete")
	}
	if value.TriggerKind == "" && value.ConversationID != "" {
		value.TriggerKind = domain.TriggerAmbient
	}
	if value.InvocationID == "" && value.ConversationID != "" {
		value.InvocationID = fmt.Sprintf("invoke_v1:%s:%d", value.ID, value.Revision)
	}
	if len(value.Checkpoint) > 0 && !json.Valid(value.Checkpoint) {
		return domain.Run{}, errors.New("Run checkpoint 不是有效 JSON")
	}
	now := time.Now()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	value.UpdatedAt = now

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var turnSession, eventID string
		if err := tx.QueryRowContext(ctx, `SELECT session_id,event_id FROM turns WHERE id=?`, value.TurnID).
			Scan(&turnSession, &eventID); err != nil {
			return mapScanError(err)
		}
		if turnSession != value.SessionID {
			return errors.New("Run 与 Turn 的 session_id 不一致")
		}
		admissionKey := ""
		if value.Lane == domain.LaneInteractive {
			var bindingJSON, payloadJSON []byte
			if err := tx.QueryRowContext(ctx,
				`SELECT binding_json,payload_json FROM inbox_events WHERE id=?`, eventID,
			).Scan(&bindingJSON, &payloadJSON); err != nil {
				return mapScanError(err)
			}
			var binding domain.ChannelBinding
			var message domain.InboundMessage
			if err := json.Unmarshal(bindingJSON, &binding); err != nil {
				return err
			}
			if err := json.Unmarshal(payloadJSON, &message); err != nil {
				return err
			}
			admissionKey = runAdmissionKey(value.SessionID, value.Lane, binding.Principal, message)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO runs(
				id,turn_id,session_id,lane,state,revision,attempt,lease_token,
				lease_until,deadline,next_attempt_at,checkpoint_json,last_error,admission_key,
					created_at,updated_at,conversation_id,current_observation_id,
					current_payload_hash,required_context_seq,trigger_kind,invocation_id
				) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`,
			value.ID,
			value.TurnID,
			value.SessionID,
			value.Lane,
			value.State,
			value.Revision,
			value.Attempt,
			value.LeaseToken,
			unixMillis(value.LeaseUntil),
			unixMillis(value.Deadline),
			unixMillis(value.NextAttempt),
			nullableJSON(value.Checkpoint),
			value.LastError,
			admissionKey,
			unixMillis(value.CreatedAt),
			unixMillis(value.UpdatedAt),
			value.ConversationID,
			value.CurrentObservationID,
			value.CurrentPayloadHash,
			value.RequiredContextSeq,
			value.TriggerKind,
			value.InvocationID,
		)
		return err
	})
	if err != nil {
		return domain.Run{}, err
	}
	return value, nil
}

func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	db, err := s.readable()
	if err != nil {
		return domain.Run{}, err
	}
	return scanRun(db.QueryRowContext(ctx,
		`SELECT `+directRunColumns+` FROM runs WHERE id=?`,
		id,
	))
}

func (s *Store) LeaseNextRun(ctx context.Context, lane domain.Lane, now time.Time, leaseDuration time.Duration) (domain.Run, error) {
	return s.leaseNextRun(ctx, lane, "", now, leaseDuration)
}

func (s *Store) LeaseNextRunByTrigger(
	ctx context.Context,
	lane domain.Lane,
	trigger domain.TriggerKind,
	now time.Time,
	leaseDuration time.Duration,
) (domain.Run, error) {
	switch trigger {
	case domain.TriggerAmbient, domain.TriggerExplicit, domain.TriggerControl:
	default:
		return domain.Run{}, storeport.ErrInvalid
	}
	return s.leaseNextRun(ctx, lane, trigger, now, leaseDuration)
}

func (s *Store) leaseNextRun(
	ctx context.Context,
	lane domain.Lane,
	trigger domain.TriggerKind,
	now time.Time,
	leaseDuration time.Duration,
) (domain.Run, error) {
	if leaseDuration <= 0 {
		return domain.Run{}, errors.New("Run lease duration 必须大于 0")
	}
	var leased domain.Run
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		triggerClause := ""
		args := []any{lane}
		if trigger != "" {
			triggerClause = " AND r.trigger_kind=?"
			args = append(args, trigger)
		}
		query := `
			SELECT ` + runColumns + `
			FROM runs r
			JOIN turns t ON t.id=r.turn_id
			JOIN inbox_events event ON event.id=t.event_id
			WHERE r.lane=?` + triggerClause + `
			  AND (
			    r.state=?
			    OR (r.state=? AND r.next_attempt_at<=?)
			  )
			  AND NOT EXISTS (
			    SELECT 1
			    FROM runs active
			    WHERE active.session_id=r.session_id
			      AND active.lane=r.lane
			      AND active.admission_key=r.admission_key
			      AND active.id<>r.id
			      AND active.state IN (?,?,?)
			  )
			  AND NOT EXISTS (
			    SELECT 1
			    FROM runs earlier
			    JOIN turns earlier_turn ON earlier_turn.id=earlier.turn_id
			    JOIN inbox_events earlier_event ON earlier_event.id=earlier_turn.event_id
			    WHERE earlier.session_id=r.session_id
			      AND earlier.lane=r.lane
			      AND earlier.admission_key=r.admission_key
			      AND earlier.id<>r.id
			      AND earlier.state IN (?,?,?,?,?,?)
			      AND (
			        earlier_event.accept_seq<event.accept_seq
			        OR (
			          earlier_event.accept_seq=event.accept_seq
			          AND (
			            earlier.created_at<r.created_at
			            OR (earlier.created_at=r.created_at AND earlier.id<r.id)
			          )
			        )
			      )
			  )
			ORDER BY t.priority DESC,r.created_at,r.id
			LIMIT 1
		`
		args = append(args,
			domain.RunQueued,
			domain.RunRetryWait,
			unixMillis(now),
			domain.RunLeased,
			domain.RunRunning,
			domain.RunCancelRequested,
			domain.RunQueued,
			domain.RunRetryWait,
			domain.RunLeased,
			domain.RunRunning,
			domain.RunCancelRequested,
			domain.RunOrphaned,
		)
		value, err := scanRun(tx.QueryRowContext(ctx, query, args...))
		if err != nil {
			return err
		}
		token, err := newID("lease")
		if err != nil {
			return err
		}
		if err := domain.ValidateRunTransition(value.State, domain.RunLeased); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		until := now.Add(leaseDuration)
		result, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET state=?,lease_token=?,lease_until=?,attempt=attempt+1,updated_at=?
			WHERE id=? AND state=?
		`,
			domain.RunLeased,
			token,
			unixMillis(until),
			unixMillis(now),
			value.ID,
			value.State,
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
		value.State = domain.RunLeased
		value.LeaseToken = token
		value.LeaseUntil = until
		value.Attempt++
		value.UpdatedAt = now
		leased = value
		return nil
	})
	return leased, err
}

func (s *Store) MarkRunRunning(ctx context.Context, id, leaseToken string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		run, err := scanRun(tx.QueryRowContext(ctx,
			`SELECT `+directRunColumns+` FROM runs WHERE id=?`,
			id,
		))
		if err != nil {
			return err
		}
		if run.State != domain.RunLeased || run.LeaseToken == "" || run.LeaseToken != leaseToken {
			return storeport.ErrConflict
		}
		if err := domain.ValidateRunTransition(run.State, domain.RunRunning); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		var turnState domain.TurnState
		if err := tx.QueryRowContext(ctx, `SELECT state FROM turns WHERE id=?`, run.TurnID).Scan(&turnState); err != nil {
			return mapScanError(err)
		}
		if turnState != domain.TurnRunning {
			if err := domain.ValidateTurnTransition(turnState, domain.TurnRunning); err != nil {
				return errors.Join(storeport.ErrConflict, err)
			}
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET state=?,updated_at=?
			WHERE id=? AND state=? AND lease_token=?
		`,
			domain.RunRunning,
			unixMillis(now),
			run.ID,
			domain.RunLeased,
			leaseToken,
		); err != nil {
			return err
		}
		if turnState != domain.TurnRunning {
			if _, err := tx.ExecContext(ctx, `
				UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?
			`,
				domain.TurnRunning,
				unixMillis(now),
				run.TurnID,
				turnState,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SaveRunCheckpoint(
	ctx context.Context,
	id string,
	leaseToken string,
	checkpoint json.RawMessage,
) error {
	if len(checkpoint) == 0 || !json.Valid(checkpoint) {
		return errors.New("run checkpoint must be valid JSON")
	}
	if len(checkpoint) > 8<<20 {
		return errors.New("run checkpoint exceeds 8 MiB")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET checkpoint_json=?,updated_at=?
			WHERE id=? AND lease_token<>'' AND lease_token=? AND state IN (?,?)
		`,
			[]byte(checkpoint),
			unixMillis(time.Now()),
			id,
			leaseToken,
			domain.RunRunning,
			domain.RunCancelRequested,
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
		return nil
	})
}

func (s *Store) RequestRunCancel(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var state domain.RunState
		var turnID string
		if err := tx.QueryRowContext(ctx, `SELECT state,turn_id FROM runs WHERE id=?`, id).Scan(&state, &turnID); err != nil {
			return mapScanError(err)
		}
		target := domain.RunCancelRequested
		if state == domain.RunQueued || state == domain.RunLeased || state == domain.RunRetryWait {
			target = domain.RunCancelled
		}
		if state == domain.RunCancelled || state == domain.RunSucceeded || state == domain.RunFailed {
			return nil
		}
		if err := domain.ValidateRunTransition(state, target); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		clearLease := target == domain.RunCancelled
		result, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET state=?,
			    lease_token=CASE WHEN ? THEN '' ELSE lease_token END,
			    lease_until=CASE WHEN ? THEN 0 ELSE lease_until END,
			    updated_at=?
			WHERE id=? AND state=?
		`,
			target,
			clearLease,
			clearLease,
			unixMillis(time.Now()),
			id,
			state,
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
		if target != domain.RunCancelled {
			return nil
		}
		if err := releaseAmbientReplyBudget(ctx, tx, id); err != nil {
			return err
		}
		return cancelTurn(ctx, tx, turnID, time.Now())
	})
}

func (s *Store) MarkRunCancelled(ctx context.Context, id, leaseToken string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		run, err := scanRun(tx.QueryRowContext(ctx,
			`SELECT `+directRunColumns+` FROM runs WHERE id=?`,
			id,
		))
		if err != nil {
			return err
		}
		if run.State != domain.RunCancelRequested || run.LeaseToken == "" || run.LeaseToken != leaseToken {
			return storeport.ErrConflict
		}
		if err := domain.ValidateRunTransition(run.State, domain.RunCancelled); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		var turnState domain.TurnState
		var eventID string
		if err := tx.QueryRowContext(ctx, `SELECT state,event_id FROM turns WHERE id=?`, run.TurnID).Scan(&turnState, &eventID); err != nil {
			return mapScanError(err)
		}
		if err := domain.ValidateTurnTransition(turnState, domain.TurnCancelled); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		now := time.Now()
		result, err := tx.ExecContext(ctx, `
			UPDATE runs SET state=?,lease_token='',lease_until=0,updated_at=?
			WHERE id=? AND state=? AND lease_token=?
		`, domain.RunCancelled, unixMillis(now), run.ID, run.State, leaseToken)
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return err
			}
			return storeport.ErrConflict
		}
		turnResult, err := tx.ExecContext(ctx, `UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?`,
			domain.TurnCancelled, unixMillis(now), run.TurnID, turnState,
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
		inboxResult, err := tx.ExecContext(ctx, `UPDATE inbox_events SET status=?,updated_at=? WHERE id=?`,
			domain.InboxDone, unixMillis(now), eventID,
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
		if err := releaseAmbientReplyBudget(ctx, tx, run.ID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) RequestSessionCancel(
	ctx context.Context,
	sessionID string,
	excludeRunID string,
) ([]string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session cancellation requires session_id")
	}
	var requested []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id,turn_id,state FROM runs
			WHERE session_id=? AND id<>? AND lane<>? AND state IN (?,?,?,?)
			ORDER BY created_at,id
		`,
			sessionID,
			excludeRunID,
			domain.LaneControl,
			domain.RunQueued,
			domain.RunLeased,
			domain.RunRunning,
			domain.RunRetryWait,
		)
		if err != nil {
			return err
		}
		type candidate struct {
			id     string
			turnID string
			state  domain.RunState
		}
		var candidates []candidate
		for rows.Next() {
			var value candidate
			if err := rows.Scan(&value.id, &value.turnID, &value.state); err != nil {
				_ = rows.Close()
				return err
			}
			candidates = append(candidates, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now()
		for _, value := range candidates {
			if value.state == domain.RunRunning {
				result, err := tx.ExecContext(ctx, `UPDATE runs SET state=?,updated_at=? WHERE id=? AND state=?`,
					domain.RunCancelRequested, unixMillis(now), value.id, value.state,
				)
				if err != nil {
					return err
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					if err != nil {
						return err
					}
					return storeport.ErrConflict
				}
				requested = append(requested, value.id)
				continue
			}
			if err := domain.ValidateRunTransition(value.state, domain.RunCancelled); err != nil {
				return errors.Join(storeport.ErrConflict, err)
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE runs SET state=?,lease_token='',lease_until=0,updated_at=? WHERE id=? AND state=?
			`, domain.RunCancelled, unixMillis(now), value.id, value.state)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				if err != nil {
					return err
				}
				return storeport.ErrConflict
			}
			if err := cancelTurn(ctx, tx, value.turnID, now); err != nil {
				return err
			}
			requested = append(requested, value.id)
		}
		return nil
	})
	return requested, err
}

func (s *Store) FailRun(
	ctx context.Context,
	id string,
	leaseToken string,
	message string,
	retryable bool,
	nextAttempt time.Time,
) error {
	target := domain.RunFailed
	if retryable {
		target = domain.RunRetryWait
		if nextAttempt.IsZero() {
			return errors.New("可重试 Run 必须提供 nextAttempt")
		}
	}
	return s.transitionLeasedRun(ctx, id, leaseToken, target, message, nextAttempt)
}

func (s *Store) transitionLeasedRun(
	ctx context.Context,
	id string,
	leaseToken string,
	target domain.RunState,
	message string,
	nextAttempt time.Time,
) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var state domain.RunState
		var storedToken string
		var turnID string
		if err := tx.QueryRowContext(ctx,
			`SELECT state,lease_token,turn_id FROM runs WHERE id=?`,
			id,
		).Scan(&state, &storedToken, &turnID); err != nil {
			return mapScanError(err)
		}
		if storedToken == "" || storedToken != leaseToken {
			return storeport.ErrConflict
		}
		if err := domain.ValidateRunTransition(state, target); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		clearLease := target != domain.RunRunning
		newToken := storedToken
		if clearLease {
			newToken = ""
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET state=?,
			    lease_token=?,
			    lease_until=CASE WHEN ? THEN 0 ELSE lease_until END,
			    next_attempt_at=?,
			    last_error=?,
			    updated_at=?
			WHERE id=? AND state=? AND lease_token=?
		`,
			target,
			newToken,
			clearLease,
			unixMillis(nextAttempt),
			message,
			unixMillis(time.Now()),
			id,
			state,
			storedToken,
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
		if target == domain.RunFailed {
			var turnState domain.TurnState
			var eventID string
			if err := tx.QueryRowContext(ctx, `SELECT state,event_id FROM turns WHERE id=?`, turnID).Scan(&turnState, &eventID); err != nil {
				return mapScanError(err)
			}
			if err := domain.ValidateTurnTransition(turnState, domain.TurnFailed); err != nil {
				return errors.Join(storeport.ErrConflict, err)
			}
			turnResult, err := tx.ExecContext(ctx, `UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?`,
				domain.TurnFailed, unixMillis(time.Now()), turnID, turnState,
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
			inboxResult, err := tx.ExecContext(ctx, `UPDATE inbox_events SET status=?,updated_at=? WHERE id=?`,
				domain.InboxFailed, unixMillis(time.Now()), eventID,
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
			if err := releaseAmbientReplyBudget(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func cancelTurn(ctx context.Context, tx *sql.Tx, turnID string, now time.Time) error {
	var turnState domain.TurnState
	var eventID string
	if err := tx.QueryRowContext(ctx, `SELECT state,event_id FROM turns WHERE id=?`, turnID).Scan(&turnState, &eventID); err != nil {
		return mapScanError(err)
	}
	if err := domain.ValidateTurnTransition(turnState, domain.TurnCancelled); err != nil {
		return errors.Join(storeport.ErrConflict, err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?`,
		domain.TurnCancelled, unixMillis(now), turnID, turnState,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return storeport.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE inbox_events SET status=?,updated_at=? WHERE id=?`,
		domain.InboxDone, unixMillis(now), eventID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return storeport.ErrConflict
	}
	return nil
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return []byte(value)
}
