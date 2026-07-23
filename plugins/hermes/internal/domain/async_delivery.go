package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type AsyncDeliveryState string

const (
	AsyncDeliveryPending       AsyncDeliveryState = "pending"
	AsyncDeliveryConsumed      AsyncDeliveryState = "consumed"
	AsyncDeliveryRevoked       AsyncDeliveryState = "revoked"
	AsyncDeliveryAbandoned     AsyncDeliveryState = "abandoned"
	MaxAsyncInvocationIDLength                    = 256
)

type AsyncDeliveryRegistration struct {
	TicketHash      string
	Profile         string
	ProducerEpoch   string
	DelegationID    string
	HermesSessionID string
	RelaySessionKey string
	ChatID          string
	ParentRunID     string
}

func (r AsyncDeliveryRegistration) Validate() error {
	values := []string{
		r.TicketHash, r.Profile, r.ProducerEpoch, r.DelegationID,
		r.HermesSessionID, r.RelaySessionKey, r.ChatID, r.ParentRunID,
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return errors.New("async delivery registration has an empty binding")
		}
	}
	return nil
}

type AsyncDeliveryTicket struct {
	ID              string
	TicketHash      string
	Profile         string
	ProducerEpoch   string
	DelegationID    string
	HermesSessionID string
	RelaySessionKey string
	ChatID          string
	SessionID       string
	ReceiverID      string
	Binding         ChannelBinding
	ParentRunID     string
	State           AsyncDeliveryState
	ResultMessageID string
	OutboxID        string
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type AsyncDeliveryCommit struct {
	TicketHash      string
	Profile         string
	ProducerEpoch   string
	DelegationID    string
	HermesSessionID string
	RelaySessionKey string
	ChatID          string
	Content         string
	Outputs         []AsyncOutput
	Silent          bool
}

type AsyncOutput struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func (c AsyncDeliveryCommit) Validate() error {
	registration := AsyncDeliveryRegistration{
		TicketHash: c.TicketHash, Profile: c.Profile,
		ProducerEpoch: c.ProducerEpoch, DelegationID: c.DelegationID,
		HermesSessionID: c.HermesSessionID,
		RelaySessionKey: c.RelaySessionKey, ChatID: c.ChatID,
		ParentRunID: "validated-by-ticket",
	}
	if err := registration.Validate(); err != nil {
		return err
	}
	if c.Silent {
		return nil
	}
	if len(c.Outputs) == 0 && strings.TrimSpace(c.Content) == "" {
		return errors.New("async delivery output is empty")
	}
	for _, output := range c.Outputs {
		if err := output.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (o AsyncOutput) Validate() error {
	if strings.TrimSpace(o.Kind) == "" {
		return errors.New("async output kind is empty")
	}
	if len(o.Payload) == 0 || !json.Valid(o.Payload) {
		return errors.New("async output payload must be valid JSON")
	}
	return nil
}

type AsyncDeliveryResult struct {
	State         AsyncDeliveryState `json:"state"`
	Disposition   string             `json:"disposition"`
	DeliveryState string             `json:"delivery_state"`
	MessageID     string             `json:"message_id"`
	OutboxID      string             `json:"outbox_id"`
	OutboxIDs     []string           `json:"outbox_ids,omitempty"`
}

type AsyncDirectOutputCommit struct {
	TicketHash      string
	Profile         string
	ProducerEpoch   string
	DelegationID    string
	HermesSessionID string
	RelaySessionKey string
	ChatID          string
	InvocationID    string
	Output          AsyncOutput
}

func (c AsyncDirectOutputCommit) Validate() error {
	binding := AsyncDeliveryCommit{
		TicketHash: c.TicketHash, Profile: c.Profile,
		ProducerEpoch: c.ProducerEpoch, DelegationID: c.DelegationID,
		HermesSessionID: c.HermesSessionID,
		RelaySessionKey: c.RelaySessionKey, ChatID: c.ChatID,
		Silent: true,
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	invocationID := strings.TrimSpace(c.InvocationID)
	if invocationID == "" {
		return errors.New("async direct output invocation id is empty")
	}
	if invocationID != c.InvocationID {
		return errors.New("async direct output invocation id has surrounding whitespace")
	}
	if len(invocationID) > MaxAsyncInvocationIDLength {
		return errors.New("async direct output invocation id is too long")
	}
	return c.Output.Validate()
}

type AsyncDirectOutputResult struct {
	Queued            bool   `json:"queued"`
	OutboxID          string `json:"outbox_id"`
	Sequence          int64  `json:"sequence"`
	DirectOutputCount int64  `json:"direct_output_count"`
}

func AsyncDeliveryPayload(
	ticket AsyncDeliveryTicket,
	content string,
	outputs []AsyncOutput,
) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"text":              content,
		"outputs":           outputs,
		"speaker_id":        "hermes",
		"speaker_name":      "Hermes async delegation",
		"delegation_id":     ticket.DelegationID,
		"hermes_session_id": ticket.HermesSessionID,
		"parent_run_id":     ticket.ParentRunID,
	})
}
