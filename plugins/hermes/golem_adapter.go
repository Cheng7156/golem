package main

import (
	"errors"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
	"google.golang.org/protobuf/proto"
)

func (p *HermesPlugin) GetSubscriptions() []string {
	return []string{
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
}

func (p *HermesPlugin) OnEvent(event *plugin.Event) (bool, error) {
	payload, ok := event.GetPayload().(*plugin.Event_Message)
	if !ok || payload.Message == nil {
		return false, nil
	}
	p.lifecycleMu.Lock()
	manager := p.config
	p.lifecycleMu.Unlock()
	if manager == nil {
		return false, errors.New("Hermes Kernel 尚未启动")
	}
	snapshot := manager.Current()
	inbox, ok, err := p.normalizeMessage(payload.Message, snapshot.BotNames)
	if err != nil || !ok {
		return false, err
	}
	if err := p.acceptInbox(inbox); err != nil {
		return false, err
	}
	return true, nil
}

func (p *HermesPlugin) normalizeMessage(
	msg *message.Message,
	botNames []string,
) (domain.InboxEvent, bool, error) {
	text := strings.TrimSpace(messageText(msg))
	media, err := inboundMedia(msg)
	if err != nil {
		return domain.InboxEvent{}, false, err
	}
	if (text == "" && len(media) == 0) || msg.GetSender() == nil || msg.GetSender().GetUsername() == "" {
		return domain.InboxEvent{}, false, nil
	}
	if msg.GetSender().GetType() == contact.ContactType_CONTACT_TYPE_SPECIAL {
		return domain.InboxEvent{}, false, nil
	}
	self, ownerID := p.identitySnapshot()
	speaker, ok := resolveMessageSpeaker(msg, ownerID)
	if !ok {
		return domain.InboxEvent{}, false, nil
	}
	if selfID := strings.TrimSpace(self.GetUsername()); selfID != "" && speaker.id == selfID {
		return domain.InboxEvent{}, false, nil
	}
	occurredAt := messageTime(msg.GetTimestamp())
	mentions := classifyMentions(msg, self, botNames)
	incoming := domain.InboundMessage{
		Text:            text,
		IsChatroom:      speaker.chatroom,
		Mentioned:       mentions.self,
		MentionedOthers: mentions.others,
		Quoted:          quotedSelf(msg, self, botNames),
		SpeakerID:       speaker.id,
		SpeakerName:     speaker.name,
		RoomName:        speaker.roomName,
		OccurredAt:      occurredAt,
		Media:           media,
	}
	inbox, err := newWechatInboxEvent(msg, speaker, incoming)
	if err != nil {
		return domain.InboxEvent{}, false, err
	}
	return inbox, true, nil
}

func (p *HermesPlugin) identitySnapshot() (*contact.SelfInfo, string) {
	p.identityMu.RLock()
	self, ownerID, updatedAt := p.self, p.ownerID, p.identityUpdatedAt
	p.identityMu.RUnlock()
	if self != nil && time.Since(updatedAt) < identityRefreshTTL {
		return self, ownerID
	}
	p.refreshIdentity()
	p.identityMu.RLock()
	defer p.identityMu.RUnlock()
	return p.self, p.ownerID
}

const identityRefreshTTL = 30 * time.Second

func (p *HermesPlugin) refreshIdentity() {
	if p.contact == nil {
		return
	}
	p.identityRefreshMu.Lock()
	defer p.identityRefreshMu.Unlock()
	p.identityMu.RLock()
	fresh := p.self != nil && time.Since(p.identityUpdatedAt) < identityRefreshTTL
	p.identityMu.RUnlock()
	if fresh {
		return
	}
	self := p.contact.GetSelf()
	owner := p.contact.GetOwner()
	ownerID := strings.TrimSpace(owner.GetUsername())
	p.identityMu.Lock()
	if self != nil {
		p.self = proto.Clone(self).(*contact.SelfInfo)
	}
	if ownerID != "" {
		p.ownerID = ownerID
	}
	if self != nil || ownerID != "" {
		p.identityUpdatedAt = time.Now()
	}
	p.identityMu.Unlock()
}
