package sqlite

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func (s *Store) ListRecentInboundContext(
	ctx context.Context,
	sessionID string,
	beforeAcceptSeq int64,
	limit int,
) ([]domain.ContextMessage, error) {
	db, err := s.readable()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 12
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.id,e.message_id,e.accept_seq,e.occurred_at,e.binding_json,e.payload_json,COALESCE(t.route,'')
		FROM inbox_events e
		LEFT JOIN turns t ON t.event_id=e.id
		WHERE e.session_id=? AND e.accept_seq<?
		ORDER BY e.accept_seq DESC
		LIMIT ?
	`, sessionID, beforeAcceptSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]domain.ContextMessage, 0, limit)
	for rows.Next() {
		var value domain.ContextMessage
		var platformMessageID int64
		var occurredAt int64
		var binding, payload []byte
		if err := rows.Scan(&value.EventID, &platformMessageID, &value.AcceptSeq, &occurredAt, &binding, &payload, &value.Route); err != nil {
			return nil, err
		}
		if platformMessageID != 0 {
			value.PlatformMessageID = strconv.FormatInt(platformMessageID, 10)
		}
		if err := json.Unmarshal(binding, &value.Binding); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &value.Message); err != nil {
			return nil, err
		}
		value.OccurredAt = fromUnixMillis(occurredAt)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
	return values, nil
}

func (s *Store) HasNewerInboundFromSpeaker(
	ctx context.Context,
	sessionID string,
	afterAcceptSeq int64,
	speakerID string,
	until time.Time,
) (bool, error) {
	db, err := s.readable()
	if err != nil {
		return false, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT payload_json
		FROM inbox_events
		WHERE session_id=? AND accept_seq>? AND occurred_at<=?
		ORDER BY accept_seq
		LIMIT 32
	`, sessionID, afterAcceptSeq, unixMillis(until))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return false, err
		}
		var message domain.InboundMessage
		if json.Unmarshal(payload, &message) == nil && message.SpeakerID == speakerID {
			return true, nil
		}
	}
	return false, rows.Err()
}
