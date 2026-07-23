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

func (s *Store) CreateTurn(ctx context.Context, value domain.Turn) (domain.Turn, error) {
	if strings.TrimSpace(value.EventID) == "" || strings.TrimSpace(value.SessionID) == "" {
		return domain.Turn{}, errors.New("Turn 缺少 event_id 或 session_id")
	}
	if value.ID == "" {
		id, err := newID("turn")
		if err != nil {
			return domain.Turn{}, err
		}
		value.ID = id
	}
	if value.State == "" {
		value.State = domain.TurnAccepted
	}
	if value.State != domain.TurnAccepted {
		return domain.Turn{}, fmt.Errorf("新 Turn 必须为 accepted，实际为 %s", value.State)
	}
	now := time.Now()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	value.UpdatedAt = now

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO turns(
				id,event_id,session_id,state,route,priority,base_session_version,
				created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?)
		`,
			value.ID,
			value.EventID,
			value.SessionID,
			value.State,
			value.Route,
			value.Priority,
			value.BaseSessionVersion,
			unixMillis(value.CreatedAt),
			unixMillis(value.UpdatedAt),
		)
		return err
	})
	if err != nil {
		return domain.Turn{}, err
	}
	return value, nil
}

func (s *Store) MaterializeTurn(ctx context.Context, eventID string, priority int) (domain.Turn, error) {
	var turn domain.Turn
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var sessionID string
		var inboxState domain.InboxStatus
		if err := tx.QueryRowContext(ctx,
			`SELECT session_id,status FROM inbox_events WHERE id=?`,
			eventID,
		).Scan(&sessionID, &inboxState); err != nil {
			return mapScanError(err)
		}
		existing, err := scanTurn(tx.QueryRowContext(ctx,
			`SELECT `+turnColumns+` FROM turns WHERE event_id=?`,
			eventID,
		))
		if err == nil {
			turn = existing
			return nil
		}
		if !errors.Is(err, storeport.ErrNotFound) {
			return err
		}
		if inboxState != domain.InboxAccepted {
			return storeport.ErrConflict
		}
		id, err := newID("turn")
		if err != nil {
			return err
		}
		now := time.Now()
		turn = domain.Turn{
			ID:        id,
			EventID:   eventID,
			SessionID: sessionID,
			State:     domain.TurnOrdered,
			Priority:  priority,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO turns(
				id,event_id,session_id,state,route,priority,base_session_version,
				created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?)
		`,
			turn.ID,
			turn.EventID,
			turn.SessionID,
			turn.State,
			turn.Route,
			turn.Priority,
			turn.BaseSessionVersion,
			unixMillis(turn.CreatedAt),
			unixMillis(turn.UpdatedAt),
		); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE inbox_events SET status=?,updated_at=? WHERE id=? AND status=?`,
			domain.InboxOrdered,
			unixMillis(now),
			eventID,
			domain.InboxAccepted,
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
	return turn, err
}

func (s *Store) GetTurn(ctx context.Context, id string) (domain.Turn, error) {
	db, err := s.readable()
	if err != nil {
		return domain.Turn{}, err
	}
	return scanTurn(db.QueryRowContext(ctx,
		`SELECT `+turnColumns+` FROM turns WHERE id=?`,
		id,
	))
}

func (s *Store) ListTurns(ctx context.Context, state domain.TurnState, limit int) ([]domain.Turn, error) {
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+turnColumns+` FROM turns WHERE state=? ORDER BY priority DESC,created_at LIMIT ?`,
		state,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []domain.Turn
	for rows.Next() {
		value, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) RouteTurn(
	ctx context.Context,
	turnID string,
	route domain.Route,
	lane domain.Lane,
	deadline time.Time,
) (domain.Turn, *domain.Run, error) {
	return s.RouteTurnWithContextDisposition(ctx, turnID, route, lane, deadline, false)
}

func (s *Store) RouteTurnWithContextDisposition(
	ctx context.Context,
	turnID string,
	route domain.Route,
	lane domain.Lane,
	deadline time.Time,
	suppressObservation bool,
) (domain.Turn, *domain.Run, error) {
	var turn domain.Turn
	var run *domain.Run
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		value, err := scanTurn(tx.QueryRowContext(ctx,
			`SELECT `+turnColumns+` FROM turns WHERE id=?`,
			turnID,
		))
		if err != nil {
			return err
		}
		if value.State != domain.TurnOrdered {
			return storeport.ErrConflict
		}
		if suppressObservation && route != domain.RouteObserve {
			return storeport.ErrInvalid
		}
		if suppressObservation {
			if err := suppressContextObservation(tx, value.EventID, time.Now()); err != nil {
				return err
			}
		}
		if err := domain.ValidateTurnTransition(value.State, domain.TurnRouted); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		now := time.Now()
		target := domain.TurnObserved
		switch route {
		case domain.RouteObserve:
			target = domain.TurnObserved
		case domain.RouteChat, domain.RouteControl:
			target = domain.TurnQueuedInteractive
			if lane == "" {
				lane = domain.LaneInteractive
			}
		case domain.RouteJob:
			target = domain.TurnQueuedJob
			if lane == "" {
				lane = domain.LaneJob
			}
		default:
			return errors.New("RouteTurn 缺少有效 route")
		}
		if err := domain.ValidateTurnTransition(domain.TurnRouted, target); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE turns SET state=?,route=?,updated_at=? WHERE id=? AND state=?`,
			domain.TurnRouted,
			route,
			unixMillis(now),
			value.ID,
			domain.TurnOrdered,
		); err != nil {
			return err
		}
		if route != domain.RouteObserve {
			switch lane {
			case domain.LaneControl, domain.LaneInteractive, domain.LaneJob, domain.LaneBackground:
			default:
				return fmt.Errorf("不支持的 lane: %s", lane)
			}
			runID, err := newID("run")
			if err != nil {
				return err
			}
			created := domain.Run{
				ID:        runID,
				TurnID:    value.ID,
				SessionID: value.SessionID,
				Lane:      lane,
				State:     domain.RunQueued,
				Deadline:  deadline,
				CreatedAt: now,
				UpdatedAt: now,
			}
			var bindingJSON, payloadJSON []byte
			var acceptSeq int64
			if err := tx.QueryRowContext(ctx,
				`SELECT binding_json,payload_json,accept_seq FROM inbox_events WHERE id=?`,
				value.EventID,
			).Scan(&bindingJSON, &payloadJSON, &acceptSeq); err != nil {
				return err
			}
			var binding domain.ChannelBinding
			if err := json.Unmarshal(bindingJSON, &binding); err != nil {
				return err
			}
			var message domain.InboundMessage
			if err := json.Unmarshal(payloadJSON, &message); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx,
				`SELECT id,payload_hash,conversation_id,conversation_seq FROM context_outbox WHERE event_id=?`,
				value.EventID,
			).Scan(&created.CurrentObservationID, &created.CurrentPayloadHash,
				&created.ConversationID, &created.RequiredContextSeq); err != nil {
				return err
			}
			_ = acceptSeq
			created.TriggerKind = domain.TriggerAmbient
			if route == domain.RouteControl {
				created.TriggerKind = domain.TriggerControl
			} else if message.Explicit() {
				created.TriggerKind = domain.TriggerExplicit
			}
			created.InvocationID = fmt.Sprintf("invoke_v1:%s:%d", created.ID, created.Revision)
			if _, err := tx.ExecContext(ctx, `
					INSERT INTO runs(
						id,turn_id,session_id,lane,state,revision,attempt,lease_token,
						lease_until,deadline,next_attempt_at,checkpoint_json,last_error,
						created_at,updated_at,conversation_id,current_observation_id,
						current_payload_hash,required_context_seq,trigger_kind,invocation_id
					) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			`,
				created.ID,
				created.TurnID,
				created.SessionID,
				created.Lane,
				created.State,
				created.Revision,
				created.Attempt,
				created.LeaseToken,
				0,
				unixMillis(created.Deadline),
				0,
				nil,
				"",
				unixMillis(created.CreatedAt),
				unixMillis(created.UpdatedAt),
				created.ConversationID,
				created.CurrentObservationID,
				created.CurrentPayloadHash,
				created.RequiredContextSeq,
				created.TriggerKind,
				created.InvocationID,
			); err != nil {
				return err
			}
			run = &created
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE turns SET state=?,updated_at=? WHERE id=? AND state=?`,
			target,
			unixMillis(now),
			value.ID,
			domain.TurnRouted,
		); err != nil {
			return err
		}
		inboxTarget := domain.InboxRouted
		if route == domain.RouteObserve {
			inboxTarget = domain.InboxDone
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE inbox_events SET status=?,updated_at=? WHERE id=?`,
			inboxTarget,
			unixMillis(now),
			value.EventID,
		); err != nil {
			return err
		}
		value.State = target
		value.Route = route
		value.UpdatedAt = now
		turn = value
		return nil
	})
	return turn, run, err
}

func suppressContextObservation(tx *sql.Tx, eventID string, now time.Time) error {
	var state domain.ContextOutboxState
	var raw []byte
	if err := tx.QueryRow(`SELECT state,observation_json FROM context_outbox WHERE event_id=?`, eventID).
		Scan(&state, &raw); err != nil {
		return mapScanError(err)
	}
	if state == domain.ContextAcked {
		// An observation acknowledged by an older dispatcher cannot be retracted.
		// Complete routing so an upgrade does not strand the Turn.
		return nil
	}
	if state == domain.ContextLeased {
		return storeport.ErrConflict
	}
	if state != domain.ContextPending && state != domain.ContextRetryWait {
		return nil
	}
	var observation domain.ConversationObservation
	if err := json.Unmarshal(raw, &observation); err != nil {
		return err
	}
	if observation.TranscriptDisposition == "ignore" {
		return nil
	}
	observation.TranscriptDisposition = "ignore"
	if err := domain.FinalizeObservationHash(&observation); err != nil {
		return err
	}
	updated, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE context_outbox SET payload_hash=?,observation_json=?,updated_at=?
		WHERE event_id=? AND state=?`, observation.PayloadHash, updated, unixMillis(now), eventID, state)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return storeport.ErrConflict
	}
	return nil
}

func (s *Store) TransitionTurn(ctx context.Context, id string, to domain.TurnState, route domain.Route) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var from domain.TurnState
		if err := tx.QueryRowContext(ctx, `SELECT state FROM turns WHERE id=?`, id).Scan(&from); err != nil {
			return mapScanError(err)
		}
		if err := domain.ValidateTurnTransition(from, to); err != nil {
			return errors.Join(storeport.ErrConflict, err)
		}
		if to == domain.TurnRouted && route == domain.RouteUnset {
			return errors.New("Turn routed 状态必须提供 route")
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE turns
			SET state=?,
			    route=CASE WHEN ?='' THEN route ELSE ? END,
			    updated_at=?
			WHERE id=? AND state=?
		`,
			to,
			route,
			route,
			unixMillis(time.Now()),
			id,
			from,
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
