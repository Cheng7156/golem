package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Principal struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	IsOwner bool   `json:"is_owner"`
}

type InboundMessage struct {
	Text            string         `json:"text"`
	HermesCommand   string         `json:"hermes_command,omitempty"`
	IsChatroom      bool           `json:"is_chatroom"`
	Mentioned       bool           `json:"mentioned"`
	MentionedOthers bool           `json:"mentioned_others,omitempty"`
	Quoted          bool           `json:"quoted"`
	SpeakerID       string         `json:"speaker_id"`
	SpeakerName     string         `json:"speaker_name,omitempty"`
	RoomName        string         `json:"room_name,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
	Media           []InboundMedia `json:"media,omitempty"`
}

type InboundMedia struct {
	Kind           string `json:"kind"`
	URL            string `json:"url,omitempty"`
	Data           []byte `json:"data,omitempty"`
	MIMEType       string `json:"mime_type,omitempty"`
	MD5            string `json:"md5,omitempty"`
	DownloadSource []byte `json:"download_source,omitempty"`
}

func (m InboundMessage) Explicit() bool {
	return strings.TrimSpace(m.HermesCommand) != "" || !m.IsChatroom || m.Mentioned || m.Quoted
}

type TextOutput struct {
	Content string `json:"content"`
}

type ImageOutput struct {
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Alt      string `json:"alt,omitempty"`
}

type EmojiOutput struct {
	URL         string `json:"url,omitempty"`
	Data        []byte `json:"data,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	MD5         string `json:"md5,omitempty"`
	Description string `json:"description,omitempty"`
}

type ChannelBinding struct {
	Channel    string    `json:"channel"`
	SessionID  string    `json:"session_id"`
	ReceiverID string    `json:"receiver_id"`
	Principal  Principal `json:"principal"`
}

func (b ChannelBinding) Validate() error {
	if strings.TrimSpace(b.Channel) == "" {
		return errors.New("channel 不能为空")
	}
	if strings.TrimSpace(b.SessionID) == "" {
		return errors.New("session_id 不能为空")
	}
	if strings.TrimSpace(b.ReceiverID) == "" {
		return errors.New("receiver_id 不能为空")
	}
	if strings.TrimSpace(b.Principal.ID) == "" {
		return errors.New("principal.id 不能为空")
	}
	return nil
}

type InboxEvent struct {
	ID         string
	DedupeKey  string
	MessageID  int64
	Topic      string
	SessionID  string
	OccurredAt time.Time
	AcceptedAt time.Time
	Binding    ChannelBinding
	Payload    json.RawMessage
	Status     InboxStatus
	AcceptSeq  int64
}

type ContextMessage struct {
	AcceptSeq  int64
	OccurredAt time.Time
	Binding    ChannelBinding
	Message    InboundMessage
	Route      Route
}

func (e InboxEvent) Validate() error {
	if strings.TrimSpace(e.ID) == "" {
		return errors.New("event id 不能为空")
	}
	if strings.TrimSpace(e.DedupeKey) == "" {
		return errors.New("dedupe_key 不能为空")
	}
	if strings.TrimSpace(e.Topic) == "" {
		return errors.New("topic 不能为空")
	}
	if strings.TrimSpace(e.SessionID) == "" {
		return errors.New("session_id 不能为空")
	}
	if err := e.Binding.Validate(); err != nil {
		return err
	}
	if e.Binding.SessionID != e.SessionID {
		return errors.New("binding.session_id 与 event.session_id 不一致")
	}
	if !json.Valid(e.Payload) {
		return errors.New("payload 不是有效 JSON")
	}
	return nil
}

type Turn struct {
	ID                 string
	EventID            string
	SessionID          string
	State              TurnState
	Route              Route
	Priority           int
	BaseSessionVersion uint64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Run struct {
	ID                   string
	TurnID               string
	SessionID            string
	Lane                 Lane
	State                RunState
	Revision             int
	ConversationID       string
	CurrentObservationID string
	CurrentPayloadHash   string
	RequiredContextSeq   int64
	TriggerKind          TriggerKind
	InvocationID         string
	Attempt              int
	LeaseToken           string
	LeaseUntil           time.Time
	Deadline             time.Time
	NextAttempt          time.Time
	Checkpoint           json.RawMessage
	LastError            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type OutboxDraft struct {
	SessionID  string
	ReceiverID string
	Kind       string
	Payload    json.RawMessage
}

func (d OutboxDraft) Validate() error {
	if strings.TrimSpace(d.SessionID) == "" {
		return errors.New("outbox session_id 不能为空")
	}
	if strings.TrimSpace(d.ReceiverID) == "" {
		return errors.New("outbox receiver_id 不能为空")
	}
	if strings.TrimSpace(d.Kind) == "" {
		return errors.New("outbox kind 不能为空")
	}
	if !json.Valid(d.Payload) {
		return errors.New("outbox payload 不是有效 JSON")
	}
	return nil
}

type OutboxItem struct {
	ID          string
	RunID       string
	SessionID   string
	ReceiverID  string
	Kind        string
	Payload     json.RawMessage
	Sequence    int64
	State       OutboxState
	Attempt     int
	LeaseToken  string
	LeaseUntil  time.Time
	NextAttempt time.Time
	ReceiptID   uint64
	ReceiptTime time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type DeliveryAttempt struct {
	ID          int64
	OutboxID    string
	Attempt     int
	Outcome     string
	Error       string
	ReceiptID   uint64
	ReceiptTime time.Time
	CreatedAt   time.Time
}

type RecoveryResult struct {
	RunsRecovered   int64
	OutboxRecovered int64
}
