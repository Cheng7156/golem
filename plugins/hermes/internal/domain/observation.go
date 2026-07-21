package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type TriggerKind string

const (
	TriggerAmbient  TriggerKind = "ambient"
	TriggerExplicit TriggerKind = "explicit"
	TriggerControl  TriggerKind = "control"
)

type VerifiedActor struct {
	ActorID       string `json:"actor_id"`
	DisplayName   string `json:"display_name,omitempty"`
	Role          string `json:"role"`
	ActorKind     string `json:"actor_kind"`
	VerifiedBy    string `json:"verified_by"`
	IdentityEpoch string `json:"identity_epoch,omitempty"`
}

type Addressing struct {
	Self             bool     `json:"self"`
	Others           bool     `json:"others"`
	QuotedSelf       bool     `json:"quoted_self"`
	MentionTargetIDs []string `json:"mention_target_ids"`
}

type ReplyContext struct {
	MessageID *string `json:"message_id"`
	ActorID   *string `json:"actor_id"`
	Text      *string `json:"text"`
}

type ObservationContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ObservationMedia struct {
	Kind                  string `json:"kind"`
	MIMEType              string `json:"mime_type,omitempty"`
	URL                   string `json:"url,omitempty"`
	MD5                   string `json:"md5,omitempty"`
	MaterializationStatus string `json:"materialization_status"`
}

type ConversationObservation struct {
	ObservationID     string             `json:"observation_id"`
	ConversationID    string             `json:"conversation_id,omitempty"`
	AcceptSeq         int64              `json:"accept_seq"`
	ConversationSeq   int64              `json:"conversation_seq"`
	EventID           string             `json:"event_id"`
	PlatformMessageID string             `json:"platform_message_id,omitempty"`
	OccurredAt        time.Time          `json:"occurred_at"`
	AcceptedAt        time.Time          `json:"accepted_at"`
	VerifiedActor     VerifiedActor      `json:"verified_actor"`
	Addressing        Addressing         `json:"addressing"`
	ReplyContext      ReplyContext       `json:"reply_context"`
	Content           ObservationContent `json:"content"`
	Media             []ObservationMedia `json:"media"`
	PayloadHash       string             `json:"payload_hash,omitempty"`
}

type ContextOutboxState string

const (
	ContextPending    ContextOutboxState = "pending"
	ContextLeased     ContextOutboxState = "leased"
	ContextRetryWait  ContextOutboxState = "retry_wait"
	ContextAcked      ContextOutboxState = "acked"
	ContextConflict   ContextOutboxState = "conflict"
	ContextDeadLetter ContextOutboxState = "dead_letter"
)

type ContextOutboxItem struct {
	ID              string
	ConversationID  string
	AcceptSeq       int64
	ConversationSeq int64
	EventID         string
	Observation     ConversationObservation
	State           ContextOutboxState
	Attempt         int
	LeaseToken      string
	LeaseUntil      time.Time
	NextAttempt     time.Time
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type ObservationBatch struct {
	RequestID            string                    `json:"request_id"`
	BatchID              string                    `json:"batch_id"`
	ConversationID       string                    `json:"conversation_id"`
	FirstConversationSeq int64                     `json:"first_conversation_seq"`
	LastConversationSeq  int64                     `json:"last_conversation_seq"`
	BatchHash            string                    `json:"batch_hash"`
	Observations         []ConversationObservation `json:"observations"`
	LeaseToken           string                    `json:"-"`
	Attempt              int                       `json:"-"`
}

type ObservationAck struct {
	RequestID                     string               `json:"request_id"`
	BatchID                       string               `json:"batch_id"`
	ConversationID                string               `json:"conversation_id"`
	BatchHash                     string               `json:"batch_hash"`
	Status                        string               `json:"status"`
	DurableThroughConversationSeq int64                `json:"durable_through_conversation_seq"`
	HighestSeenConversationSeq    int64                `json:"highest_seen_conversation_seq"`
	ErrorCode                     string               `json:"error_code,omitempty"`
	Items                         []ObservationAckItem `json:"items,omitempty"`
	Gaps                          []int64              `json:"gaps,omitempty"`
}

type ObservationAckItem struct {
	ObservationID string `json:"observation_id"`
	Status        string `json:"status"`
}

func StableConversationID(binding ChannelBinding) string {
	kind := "dm"
	if strings.HasPrefix(binding.SessionID, "chatroom:") {
		kind = "group"
	}
	value := strings.TrimPrefix(strings.TrimPrefix(binding.SessionID, "chatroom:"), "private:")
	return fmt.Sprintf("%s:%s:%s", binding.Channel, kind, value)
}

func ObservationID(event InboxEvent) string {
	sum := sha256.Sum256([]byte(event.Binding.Channel + "\x00" + StableConversationID(event.Binding) + "\x00" + event.DedupeKey))
	return "obs_v1:" + hex.EncodeToString(sum[:])
}

func NewConversationObservation(event InboxEvent) (ConversationObservation, error) {
	var message InboundMessage
	if err := json.Unmarshal(event.Payload, &message); err != nil {
		return ConversationObservation{}, err
	}
	role := "participant_not_owner"
	if event.Binding.Principal.IsOwner {
		role = "owner_of_this_agent"
	}
	contentType := "text"
	if len(message.Media) > 0 && strings.TrimSpace(message.Media[0].Kind) != "" {
		contentType = strings.TrimSpace(message.Media[0].Kind)
	}
	platformMessageID := ""
	if event.MessageID != 0 {
		platformMessageID = fmt.Sprintf("%d", event.MessageID)
	}
	media := make([]ObservationMedia, 0, len(message.Media))
	for _, item := range message.Media {
		status := "metadata_only"
		if len(item.Data) > 0 {
			status = "available_at_ingress"
		} else if len(item.DownloadSource) > 0 {
			status = "deferred"
		}
		media = append(media, ObservationMedia{Kind: item.Kind, MIMEType: item.MIMEType,
			URL: item.URL, MD5: item.MD5, MaterializationStatus: status})
	}
	observation := ConversationObservation{
		ObservationID: ObservationID(event), ConversationID: StableConversationID(event.Binding),
		AcceptSeq: event.AcceptSeq, EventID: event.ID,
		PlatformMessageID: platformMessageID, OccurredAt: event.OccurredAt,
		AcceptedAt: event.AcceptedAt,
		VerifiedActor: VerifiedActor{ActorID: event.Binding.Principal.ID, DisplayName: event.Binding.Principal.Name,
			Role: role, ActorKind: "unknown", VerifiedBy: "golem_wechat_protocol"},
		Addressing: Addressing{Self: message.Mentioned, Others: message.MentionedOthers,
			QuotedSelf: message.Quoted, MentionTargetIDs: []string{}},
		ReplyContext: ReplyContext{}, Content: ObservationContent{Type: contentType, Text: message.Text},
		Media: media,
	}
	if err := FinalizeObservationHash(&observation); err != nil {
		return ConversationObservation{}, err
	}
	return observation, nil
}

func FinalizeObservationHash(observation *ConversationObservation) error {
	if observation == nil {
		return errors.New("observation is nil")
	}
	observation.PayloadHash = ""
	canonical, err := CanonicalJSON(observation)
	if err != nil {
		return err
	}
	payloadHash := sha256.Sum256(canonical)
	observation.PayloadHash = hex.EncodeToString(payloadHash[:])
	return nil
}

func CanonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func FinalizeBatchHash(batch *ObservationBatch) error {
	if batch == nil || len(batch.Observations) == 0 {
		return errors.New("observation batch is empty")
	}
	payloadHashes := make([]string, 0, len(batch.Observations))
	for _, observation := range batch.Observations {
		payloadHashes = append(payloadHashes, observation.PayloadHash)
	}
	canonical, err := CanonicalJSON(map[string]any{
		"conversation_id":        batch.ConversationID,
		"first_conversation_seq": batch.FirstConversationSeq,
		"last_conversation_seq":  batch.LastConversationSeq,
		"payload_hashes":         payloadHashes,
	})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	batch.BatchHash = hex.EncodeToString(digest[:])
	return nil
}

func (b ObservationBatch) Validate() error {
	if b.BatchID == "" || b.RequestID == "" || b.ConversationID == "" || len(b.Observations) == 0 {
		return errors.New("observation batch is incomplete")
	}
	if b.FirstConversationSeq != b.Observations[0].ConversationSeq || b.LastConversationSeq != b.Observations[len(b.Observations)-1].ConversationSeq {
		return errors.New("observation batch sequence bounds are inconsistent")
	}
	previous := b.FirstConversationSeq - 1
	for _, observation := range b.Observations {
		if observation.ConversationID != b.ConversationID || observation.ObservationID == "" || observation.PayloadHash == "" || observation.ConversationSeq != previous+1 {
			return errors.New("observation batch item is invalid")
		}
		if observation.VerifiedActor.Role != "owner_of_this_agent" && observation.VerifiedActor.Role != "participant_not_owner" {
			return errors.New("observation batch actor role is invalid")
		}
		previous = observation.ConversationSeq
	}
	if b.BatchHash == "" {
		return errors.New("observation batch hash is empty")
	}
	return nil
}
