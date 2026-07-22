package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"golem_plugin_hermes/internal/config"

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
			if _, accepted, err := plugin.normalizeMessage(test.msg, nil, config.RoutingConfig{}); err != nil || accepted {
				t.Fatalf("normalizeMessage accepted self message: accepted=%v err=%v", accepted, err)
			}
		})
	}
}

func TestNormalizeMessageAcceptsOtherChatroomMember(t *testing.T) {
	plugin := newHermesPlugin()
	plugin.self = &contact.SelfInfo{Username: "wxid_bot"}
	msg := textMessage(room("room@chatroom"), &chatroom.Member{Username: "wxid_member"})

	event, accepted, err := plugin.normalizeMessage(msg, nil, config.RoutingConfig{})
	if err != nil || !accepted {
		t.Fatalf("normalizeMessage rejected member message: accepted=%v err=%v", accepted, err)
	}
	if event.SessionID != "chatroom:room@chatroom" {
		t.Fatalf("session_id=%q", event.SessionID)
	}
}

func TestNormalizeMessageClassifiesMentions(t *testing.T) {
	plugin := newHermesPlugin()
	plugin.self = &contact.SelfInfo{
		Username: "wxid_bot",
		Nickname: "ccff",
	}

	tests := []struct {
		name       string
		content    string
		reminds    []string
		botNames   []string
		wantSelf   bool
		wantOthers bool
	}{
		{
			name:     "structured self mention",
			content:  "@ccff hello",
			reminds:  []string{"wxid_bot"},
			wantSelf: true,
		},
		{
			name:       "structured other mention",
			content:    "@火 你在做什么",
			reminds:    []string{"wxid_other"},
			wantOthers: true,
		},
		{
			name:       "self and other mentions",
			content:    "@ccff @火 看一下",
			reminds:    []string{"wxid_bot", "wxid_other"},
			wantSelf:   true,
			wantOthers: true,
		},
		{
			name:     "fallback self mention",
			content:  "@ccff hello",
			wantSelf: true,
		},
		{
			name:     "bare bot name is not a mention",
			content:  "unknown command: /hermes",
			botNames: []string{"hermes"},
		},
		{
			name:       "longer account name is another mention",
			content:    "@hermes2 看一下",
			botNames:   []string{"hermes"},
			wantOthers: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := textMessage(room("room@chatroom"), &chatroom.Member{Username: "wxid_member"})
			msg.GetText().Content = test.content
			msg.GetText().Reminds = test.reminds
			event, accepted, err := plugin.normalizeMessage(msg, test.botNames, config.RoutingConfig{})
			if err != nil || !accepted {
				t.Fatalf("normalizeMessage accepted=%v err=%v", accepted, err)
			}
			var incoming struct {
				Mentioned       bool `json:"mentioned"`
				MentionedOthers bool `json:"mentioned_others"`
			}
			if err := json.Unmarshal(event.Payload, &incoming); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if incoming.Mentioned != test.wantSelf || incoming.MentionedOthers != test.wantOthers {
				t.Fatalf("mentions self=%v others=%v, want self=%v others=%v",
					incoming.Mentioned, incoming.MentionedOthers, test.wantSelf, test.wantOthers)
			}
		})
	}
}

func TestNormalizeMessagePersistsConfiguredActorKind(t *testing.T) {
	plugin := newHermesPlugin()
	plugin.self = &contact.SelfInfo{Username: "wxid_bot"}

	for _, test := range []struct {
		name     string
		member   *chatroom.Member
		routing  config.RoutingConfig
		wantKind string
	}{
		{
			name:     "configured bot id",
			member:   &chatroom.Member{Username: "wxid_ovo", DisplayName: "ovo"},
			routing:  config.RoutingConfig{AutomatedSpeakerIDs: []string{"wxid_ovo"}},
			wantKind: "bot",
		},
		{
			name:     "ordinary human",
			member:   &chatroom.Member{Username: "wxid_member", DisplayName: "Member"},
			wantKind: "human",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			msg := textMessage(room("room@chatroom"), test.member)
			event, accepted, err := plugin.normalizeMessage(msg, nil, test.routing)
			if err != nil || !accepted {
				t.Fatalf("normalizeMessage accepted=%v err=%v", accepted, err)
			}
			if event.Binding.Principal.Kind != test.wantKind {
				t.Fatalf("principal kind=%q, want %q", event.Binding.Principal.Kind, test.wantKind)
			}
		})
	}
}

func TestGetSubscriptionsIncludesSupportedInboundMessages(t *testing.T) {
	got := newHermesPlugin().GetSubscriptions()
	want := []string{
		message.TypeText.Topic,
		message.TypeImage.Topic,
		message.TypeFile.Topic,
		message.TypeVoice.Topic,
		message.TypePersonalCard.Topic,
		message.TypeVideo.Topic,
		message.TypeEmoji.Topic,
		message.TypeLocation.Topic,
		message.TypeApplication.Topic,
		message.TypeAppNote.Topic,
		message.TypeAppMiniapp.Topic,
		message.TypeAppFileNotify.Topic,
		message.TypeAppFileAttach.Topic,
		message.TypeAppChatRecord.Topic,
		message.TypeAppMusic.Topic,
		message.TypeAppLink.Topic,
		message.TypeAppQuote.Topic,
		message.TypeAppFinder.Topic,
		message.TypeTinyVideo.Topic,
		message.TypeBusinessCard.Topic,
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subscriptions=%v want=%v", got, want)
	}
}

func TestMessageTextUsesControlledUnsupportedMediaPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		msg  *message.Message
		want string
	}{
		{
			name: "voice",
			msg: &message.Message{Type: message.TypeVoice, Data: &message.Message_Voice{
				Voice: &message.VoiceData{Duration: 1234},
			}},
			want: "[voice message; duration_ms=1234]",
		},
		{
			name: "video",
			msg: &message.Message{Type: message.TypeVideo, Data: &message.Message_Video{
				Video: &message.VideoData{Duration: 12},
			}},
			want: "[video message; duration_seconds=12]",
		},
		{
			name: "location",
			msg: &message.Message{Type: message.TypeLocation, Data: &message.Message_Location{
				Location: &message.LocationData{PoiName: "West Lake", Latitude: 30.25, Longitude: 120.15},
			}},
			want: "[location: West Lake; latitude=30.250000; longitude=120.150000]",
		},
		{
			name: "file",
			msg:  &message.Message{Type: message.TypeFile, Content: "report.pdf"},
			want: "[file: report.pdf]",
		},
		{
			name: "personal card",
			msg:  &message.Message{Type: message.TypePersonalCard, Content: "Alice"},
			want: "[personal contact card: Alice]",
		},
		{
			name: "business card",
			msg:  &message.Message{Type: message.TypeBusinessCard, Content: "Acme"},
			want: "[business contact card: Acme]",
		},
		{
			name: "application",
			msg: &message.Message{Type: message.TypeAppLink, Data: &message.Message_App{
				App: &message.AppData{SubType: 5, Title: "Docs", Desc: "Reference", Url: "https://example.com/docs"},
			}},
			want: "[application message; type=5; title=Docs; description=Reference; url=https://example.com/docs]",
		},
		{name: "tiny video", msg: &message.Message{Type: message.TypeTinyVideo}, want: "[tiny video]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := messageText(test.msg); got != test.want {
				t.Fatalf("messageText=%s want=%s", fmt.Sprintf("%q", got), fmt.Sprintf("%q", test.want))
			}
		})
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
