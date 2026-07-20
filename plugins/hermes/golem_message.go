package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type messageSpeaker struct {
	chatroom  bool
	sessionID string
	id        string
	name      string
	roomName  string
	receiver  string
	isOwner   bool
}

func resolveMessageSpeaker(msg *message.Message, ownerID string) (messageSpeaker, bool) {
	sender := msg.GetSender()
	if sender == nil || strings.TrimSpace(sender.GetUsername()) == "" {
		return messageSpeaker{}, false
	}
	speaker := messageSpeaker{
		sessionID: "private:" + sender.GetUsername(),
		id:        sender.GetUsername(),
		name:      displayContact(sender),
		receiver:  sender.GetUsername(),
	}
	if sender.GetType() == contact.ContactType_CONTACT_TYPE_CHATROOM {
		speaker.chatroom = true
		speaker.sessionID = "chatroom:" + sender.GetUsername()
		speaker.id = msg.GetMember().GetUsername()
		speaker.name = displayMember(msg.GetMember())
		speaker.roomName = displayContact(sender)
	}
	if speaker.id == "" {
		return messageSpeaker{}, false
	}
	speaker.isOwner = speaker.id == ownerID
	return speaker, true
}

func newWechatInboxEvent(
	msg *message.Message,
	speaker messageSpeaker,
	incoming domain.InboundMessage,
) (domain.InboxEvent, error) {
	payload, err := json.Marshal(incoming)
	if err != nil {
		return domain.InboxEvent{}, err
	}
	id, dedupe, err := messageIdentity(msg)
	if err != nil {
		return domain.InboxEvent{}, err
	}
	return domain.InboxEvent{
		ID:         id,
		DedupeKey:  dedupe,
		MessageID:  msg.GetId(),
		Topic:      msg.GetType().GetTopic(),
		SessionID:  speaker.sessionID,
		OccurredAt: incoming.OccurredAt,
		Binding: domain.ChannelBinding{
			Channel:    "wechat",
			SessionID:  speaker.sessionID,
			ReceiverID: speaker.receiver,
			Principal: domain.Principal{
				ID:      speaker.id,
				Name:    speaker.name,
				IsOwner: speaker.isOwner,
			},
		},
		Payload: payload,
	}, nil
}

func messageIdentity(msg *message.Message) (string, string, error) {
	if msg.GetId() != 0 {
		value := fmt.Sprintf("%d", msg.GetId())
		return "event_wechat_" + value, "wechat/message/" + value, nil
	}
	data, err := protojson.Marshal(msg)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	value := hex.EncodeToString(sum[:])
	return "event_wechat_" + value, "wechat/fingerprint/" + value, nil
}

func messageText(msg *message.Message) string {
	if text := msg.GetText(); text != nil && text.GetContent() != "" {
		return text.GetContent()
	}
	if app := msg.GetApp(); app != nil {
		if app.GetTitle() != "" {
			return app.GetTitle()
		}
		return app.GetDesc()
	}
	if msg.GetImage() != nil {
		return "[image]"
	}
	if msg.GetEmoji() != nil {
		return "[sticker]"
	}
	return msg.GetContent()
}

func inboundMedia(msg *message.Message) ([]domain.InboundMedia, error) {
	if msg == nil {
		return nil, nil
	}
	kind, media := inboundMediaValue(msg)
	if media == nil {
		return nil, nil
	}
	data := append([]byte(nil), media.GetData()...)
	if len(data) > maxInboundMediaBytes {
		return nil, errors.New("inbound media exceeds 16 MiB")
	}
	mimeType := ""
	if len(data) > 0 {
		mimeType = http.DetectContentType(data)
	}
	downloadSource, err := imageDownloadSource(msg, kind, data)
	if err != nil {
		return nil, err
	}
	return []domain.InboundMedia{{
		Kind: kind, URL: strings.TrimSpace(media.GetUrl()), Data: data,
		MIMEType: mimeType, MD5: strings.TrimSpace(media.GetMd5()),
		DownloadSource: downloadSource,
	}}, nil
}

func inboundMediaValue(msg *message.Message) (string, *message.Media) {
	if msg.GetImage() != nil {
		return "image", msg.GetImage().GetMedia()
	}
	if msg.GetEmoji() != nil {
		return "emoji", msg.GetEmoji().GetMedia()
	}
	return "", nil
}

func imageDownloadSource(msg *message.Message, kind string, data []byte) ([]byte, error) {
	if len(data) > 0 || kind != "image" {
		return nil, nil
	}
	encoded, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode image download source: %w", err)
	}
	return encoded, nil
}

func mentionedSelf(msg *message.Message, self *contact.SelfInfo, botNames []string) bool {
	identities := selfIdentities(self, botNames)
	if text := msg.GetText(); text != nil {
		for _, remind := range text.GetReminds() {
			for _, part := range strings.FieldsFunc(remind, mentionSeparator) {
				part = strings.TrimPrefix(strings.TrimSpace(part), "@")
				if containsIdentity(part, identities) {
					return true
				}
			}
		}
	}
	content := strings.ToLower(messageText(msg))
	for _, identity := range identities {
		if strings.Contains(content, "@"+strings.ToLower(identity)) {
			return true
		}
	}
	for _, name := range botNames {
		if name = strings.TrimSpace(name); name != "" && strings.Contains(content, strings.ToLower(name)) {
			return true
		}
	}
	return false
}

func quotedSelf(msg *message.Message, self *contact.SelfInfo, botNames []string) bool {
	var raw string
	if app := msg.GetApp(); app != nil {
		raw = app.GetXml()
	}
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var value struct {
		AppMsg struct {
			Refer quoteRefer `xml:"refermsg"`
		} `xml:"appmsg"`
		Refer quoteRefer `xml:"refermsg"`
	}
	if xml.Unmarshal([]byte(raw), &value) != nil {
		return false
	}
	refer := value.AppMsg.Refer
	if refer.FromUser == "" && refer.ChatUser == "" && refer.DisplayName == "" {
		refer = value.Refer
	}
	identities := selfIdentities(self, botNames)
	return containsIdentity(refer.FromUser, identities) ||
		containsIdentity(refer.ChatUser, identities) ||
		containsIdentity(refer.DisplayName, identities)
}

type quoteRefer struct {
	DisplayName string `xml:"displayname"`
	FromUser    string `xml:"fromusr"`
	ChatUser    string `xml:"chatusr"`
}

func selfIdentities(self *contact.SelfInfo, botNames []string) []string {
	values := append([]string(nil), botNames...)
	if self != nil {
		values = append(values, self.GetUsername(), self.GetNickname(), self.GetAlias())
	}
	var result []string
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func containsIdentity(value string, identities []string) bool {
	value = strings.TrimSpace(value)
	for _, identity := range identities {
		if strings.EqualFold(value, identity) {
			return true
		}
	}
	return false
}

func mentionSeparator(value rune) bool {
	return unicode.IsSpace(value) || value == ',' || value == '\uFF0C' ||
		value == ';' || value == '\uFF1B'
}
