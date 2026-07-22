package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewConversationObservationCarriesPrincipalActorKind(t *testing.T) {
	payload, err := json.Marshal(InboundMessage{Text: "hello", IsChatroom: true})
	if err != nil {
		t.Fatal(err)
	}
	event := InboxEvent{
		ID: "event-1", DedupeKey: "wechat/message/1", MessageID: 1,
		SessionID: "chatroom:room", OccurredAt: time.Now(), AcceptedAt: time.Now(),
		Binding: ChannelBinding{
			Channel: "wechat", SessionID: "chatroom:room", ReceiverID: "room",
			Principal: Principal{ID: "wxid_ovo", Name: "ovo", Kind: "bot"},
		},
		Payload: payload,
	}

	observation, err := NewConversationObservation(event)
	if err != nil {
		t.Fatal(err)
	}
	if observation.VerifiedActor.ActorKind != "bot" {
		t.Fatalf("actor_kind=%q, want bot", observation.VerifiedActor.ActorKind)
	}
}

func TestObservationCanonicalHashGolden(t *testing.T) {
	occurredAt, err := time.Parse(time.RFC3339, "2026-07-21T12:34:56Z")
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt, err := time.Parse(time.RFC3339, "2026-07-21T12:34:57Z")
	if err != nil {
		t.Fatal(err)
	}
	observation := ConversationObservation{
		ObservationID:     "obs_v1:fixture_001",
		ConversationID:    "wechat:group:room_测试",
		AcceptSeq:         42,
		ConversationSeq:   7,
		EventID:           "event_wechat_9001",
		PlatformMessageID: "9001",
		OccurredAt:        occurredAt,
		AcceptedAt:        acceptedAt,
		VerifiedActor: VerifiedActor{
			ActorID:       "wxid_机器人甲",
			DisplayName:   "机器人甲",
			Role:          "participant_not_owner",
			ActorKind:     "bot",
			VerifiedBy:    "golem_wechat_protocol",
			IdentityEpoch: "owner:wxid_owner:v3",
		},
		Addressing: Addressing{
			Self:             false,
			Others:           true,
			QuotedSelf:       false,
			MentionTargetIDs: []string{"wxid_机器人乙"},
		},
		ReplyContext: ReplyContext{},
		Content: ObservationContent{
			Type: "text",
			Text: "@机器人乙 给主人看看，这不是在叫 Hermes 的主人。",
		},
		Media: []ObservationMedia{},
	}
	if err := FinalizeObservationHash(&observation); err != nil {
		t.Fatal(err)
	}
	const payloadHash = "8d85de7c5b226cb9e0717cb6de3551a3c632d24c6731018af0397ad8a2da1ea4"
	if observation.PayloadHash != payloadHash {
		t.Fatalf("payload hash = %s, want %s", observation.PayloadHash, payloadHash)
	}

	batch := ObservationBatch{
		RequestID:            "req_fixture",
		BatchID:              "batch_v1:fixture",
		ConversationID:       observation.ConversationID,
		FirstConversationSeq: 7,
		LastConversationSeq:  7,
		Observations:         []ConversationObservation{observation},
	}
	if err := FinalizeBatchHash(&batch); err != nil {
		t.Fatal(err)
	}
	const batchHash = "8f934d2c0c845c41a77e3988cd34864622fd544d6d0020a7c9041ddbd66e976c"
	if batch.BatchHash != batchHash {
		t.Fatalf("batch hash = %s, want %s", batch.BatchHash, batchHash)
	}
}
