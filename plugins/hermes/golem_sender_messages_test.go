package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/contact"
)

func TestTextOutboxMessagePreservesVerifiedGroupMention(t *testing.T) {
	payload, err := json.Marshal(domain.TextOutput{
		Content: "收到，我来看看。",
		Delivery: &domain.DeliveryTarget{
			ReplyToMessageID: "12345",
			MentionActorID:   "wxid_alice",
			MentionActorName: "Alice",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := textOutboxMessage(&contact.Contact{Username: "room@chatroom"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "@Alice 收到，我来看看。" || result.GetText().Content != result.Content {
		t.Fatalf("content=%q text=%q", result.Content, result.GetText().Content)
	}
	if !reflect.DeepEqual(result.GetText().Reminds, []string{"wxid_alice"}) {
		t.Fatalf("reminds=%#v", result.GetText().Reminds)
	}
}

func TestTextOutboxMessageDoesNotDuplicateMentionPrefix(t *testing.T) {
	payload := json.RawMessage(`{"content":"@Alice 已处理","delivery":{"mention_actor_id":"wxid_alice","mention_actor_name":"Alice"}}`)
	result, err := textOutboxMessage(&contact.Contact{Username: "room@chatroom"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "@Alice 已处理" {
		t.Fatalf("content=%q", result.Content)
	}
}
