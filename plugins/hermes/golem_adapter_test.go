package main

import (
	"testing"

	"github.com/sbgayhub/golem/sdk/chatroom"
	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

func TestNormalizeMessageRejectsSelfMessages(t *testing.T) {
	plugin := newHermesPlugin()
	plugin.self = &contact.SelfInfo{Username: "wxid_bot"}

	tests := []struct {
		name string
		msg  *message.Message
	}{
		{name: "private", msg: textMessage(friend("wxid_bot"), nil)},
		{name: "chatroom", msg: textMessage(room("room@chatroom"), &chatroom.Member{Username: "wxid_bot"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, accepted, err := plugin.normalizeMessage(test.msg, nil); err != nil || accepted {
				t.Fatalf("normalizeMessage accepted self message: accepted=%v err=%v", accepted, err)
			}
		})
	}
}

func TestNormalizeMessageAcceptsOtherChatroomMember(t *testing.T) {
	plugin := newHermesPlugin()
	plugin.self = &contact.SelfInfo{Username: "wxid_bot"}
	msg := textMessage(room("room@chatroom"), &chatroom.Member{Username: "wxid_member"})

	event, accepted, err := plugin.normalizeMessage(msg, nil)
	if err != nil || !accepted {
		t.Fatalf("normalizeMessage rejected member message: accepted=%v err=%v", accepted, err)
	}
	if event.SessionID != "chatroom:room@chatroom" {
		t.Fatalf("session_id=%q", event.SessionID)
	}
}

func textMessage(sender *contact.Contact, member *chatroom.Member) *message.Message {
	return &message.Message{
		Id:     1,
		Type:   message.TypeText,
		Sender: sender,
		Member: member,
		Data: &message.Message_Text{Text: &message.TextData{
			Content: "hello",
		}},
	}
}

func friend(username string) *contact.Contact {
	return &contact.Contact{Username: username, Type: contact.ContactType_CONTACT_TYPE_FRIEND}
}

func room(username string) *contact.Contact {
	return &contact.Contact{Username: username, Type: contact.ContactType_CONTACT_TYPE_CHATROOM}
}
