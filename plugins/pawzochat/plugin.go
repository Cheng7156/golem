package main

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
	"google.golang.org/protobuf/proto"
)

func (p *PawzoChatPlugin) GetMetadata() *plugin.Metadata {
	return &plugin.Metadata{
		Name:        "pawzochat",
		Author:      "PawzoChat",
		Version:     "0.5.0",
		Description: "将 golem 微信消息路由到 PawzoChat 角色并回传回复。",
		Priority:    1<<31 - 1,
		Next:        false,
		AlwaysRun:   false,
	}
}

func (p *PawzoChatPlugin) GetSubscriptions() []string {
	return []string{
		message.TypeText.Topic, message.TypeAppQuote.Topic,
		message.TypeImage.Topic, message.TypeEmoji.Topic,
	}
}

func (p *PawzoChatPlugin) OnLoad() error {
	p.normalizeConfig()
	p.startEmojiWorkers()
	p.startMediaWorkers()
	p.refreshIdentity()
	return nil
}

func (p *PawzoChatPlugin) OnUnload() error {
	p.stopEmojiWorkers()
	p.stopMediaWorkers()
	return nil
}

func (p *PawzoChatPlugin) OnEnable() error {
	p.normalizeConfig()
	p.startEmojiWorkers()
	p.startMediaWorkers()
	p.refreshIdentity()
	return nil
}

func (p *PawzoChatPlugin) OnDisable() error {
	p.stopEmojiWorkers()
	p.stopMediaWorkers()
	return nil
}

func (p *PawzoChatPlugin) OnConfigChange() error {
	p.normalizeConfig()
	return nil
}

func (p *PawzoChatPlugin) OnEvent(event *plugin.Event) (bool, error) {
	payload, ok := event.GetPayload().(*plugin.Event_Message)
	if !ok || payload.Message == nil {
		return false, nil
	}
	if payload.Message.GetSender().GetType() == contact.ContactType_CONTACT_TYPE_SPECIAL {
		return false, nil
	}
	config := p.configSnapshot()
	self, ownerID, ownerName := p.identityForEvent()
	if payload.Message.GetType().GetCode() == message.TypeEmoji.Code {
		if candidate, ok := buildEmojiCollectionJob(payload.Message, self, config); ok {
			p.enqueueEmojiCollection(candidate)
		}
		if mediaJob, ok := buildMediaStorageJob(
			payload.Message, self, ownerID, ownerName, config,
		); ok {
			p.enqueueMediaStorage(mediaJob)
		}
		return false, nil
	}
	if payload.Message.GetType().GetCode() == message.TypeImage.Code {
		if mediaJob, ok := buildMediaStorageJob(
			payload.Message, self, ownerID, ownerName, config,
		); ok {
			p.enqueueMediaStorage(mediaJob)
		}
		return false, nil
	}
	incoming, ok := buildIncoming(payload.Message, self, ownerID, ownerName)
	if !ok {
		return false, nil
	}
	personaID := p.personaForSession(config, incoming.SessionKey)
	if personaID == "" {
		return false, nil
	}
	if self == nil || strings.TrimSpace(self.GetUsername()) == "" {
		return true, errors.New("golem self identity is unavailable")
	}
	if strings.TrimSpace(ownerID) == "" {
		return true, errors.New("golem owner identity is unavailable")
	}
	if incoming.IsChatroom && !config.RespondToAllGroupMessages &&
		!incoming.MentionedBot && !incoming.QuotedBot {
		return false, nil
	}
	// Preserve arrival order: a following text turn must see any image ID whose
	// upload is still completing, regardless of what words the user chose.
	p.waitForPendingMedia(incoming.SessionKey, 12*time.Second)

	batch, run := p.enqueueBatch(incoming)
	if !run {
		return true, nil
	}
	return true, p.processSession(incoming.SessionKey, personaID, batch)
}

type queuedBatch struct {
	messages []incomingMessage
	explicit bool
	queuedAt time.Time
}

type sessionState struct {
	active  bool
	pending *queuedBatch
}

func (batch *queuedBatch) prompt() string {
	parts := make([]string, 0, len(batch.messages))
	for index, incoming := range batch.messages {
		content := incoming.promptContent()
		if index > 0 {
			content = "[another_message_in_same_session]\n" + content
		}
		parts = append(parts, content)
	}
	return strings.Join(parts, "\n\n")
}

func (batch *queuedBatch) representative() incomingMessage {
	return batch.messages[0]
}

func (incoming incomingMessage) isExplicit() bool {
	return !incoming.IsChatroom || incoming.MentionedBot || incoming.QuotedBot
}

func sessionType(sessionKey string) string {
	value, _, _ := strings.Cut(sessionKey, ":")
	return value
}

func (p *PawzoChatPlugin) enqueueBatch(incoming incomingMessage) (*queuedBatch, bool) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.sessions == nil {
		p.sessions = make(map[string]*sessionState)
	}
	state := p.sessions[incoming.SessionKey]
	if state == nil {
		state = &sessionState{}
		p.sessions[incoming.SessionKey] = state
	}
	if !state.active {
		state.active = true
		return &queuedBatch{
			messages: []incomingMessage{incoming}, explicit: incoming.isExplicit(), queuedAt: time.Now(),
		}, true
	}
	if state.pending == nil {
		state.pending = &queuedBatch{
			messages: []incomingMessage{incoming}, explicit: incoming.isExplicit(), queuedAt: time.Now(),
		}
	} else if incoming.isExplicit() {
		insertAt := len(state.pending.messages)
		for index, message := range state.pending.messages {
			if !message.isExplicit() {
				insertAt = index
				break
			}
		}
		state.pending.messages = append(state.pending.messages, incomingMessage{})
		copy(state.pending.messages[insertAt+1:], state.pending.messages[insertAt:])
		state.pending.messages[insertAt] = incoming
		state.pending.explicit = true
	} else {
		state.pending.messages = append(state.pending.messages, incoming)
		state.pending.explicit = state.pending.explicit || incoming.isExplicit()
	}
	slog.Info("[pawzochat] 合并会话待处理消息", "session_type", sessionType(incoming.SessionKey),
		"pending", len(state.pending.messages), "explicit", state.pending.explicit)
	return nil, false
}

func (p *PawzoChatPlugin) processSession(
	sessionKey string,
	personaID string,
	batch *queuedBatch,
) error {
	var firstErr error
	for batch != nil {
		representative := batch.representative()
		config := p.configSnapshot()
		slog.Info("[pawzochat] 开始处理会话批次", "session_type", sessionType(sessionKey),
			"messages", len(batch.messages), "queue_wait_ms", time.Since(batch.queuedAt).Milliseconds())
		outputs, noReply, err := p.requestReplyPrompt(
			config,
			personaID,
			sessionKey,
			representative.sessionName(),
			batch.prompt(),
			representative.Quote.Content,
			batch.explicit,
		)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Error("[pawzochat] PawzoChat 请求失败", "session_type", sessionType(sessionKey), "err", err)
		} else if !noReply {
			textSegments := 0
			for _, output := range outputs {
				if output.Kind == "text" {
					textSegments++
				}
			}
			slog.Info("[pawzochat] 收到 PawzoChat 回复", "session_type", sessionType(sessionKey),
				"segments", len(outputs), "text_segments", textSegments)
			if len(outputs) == 0 {
				if firstErr == nil {
					firstErr = errors.New("PawzoChat returned no deliverable content")
				}
			} else {
				for _, output := range outputs {
					if sendErr := p.sendOutput(representative.Receiver, output); sendErr != nil {
						if firstErr == nil {
							firstErr = sendErr
						}
						slog.Error("[pawzochat] 发送回复失败", "session_type", sessionType(sessionKey), "err", sendErr)
						break
					}
				}
			}
		}

		p.sessionMu.Lock()
		state := p.sessions[sessionKey]
		if state == nil || state.pending == nil {
			if state != nil {
				state.active = false
				delete(p.sessions, sessionKey)
			}
			batch = nil
		} else {
			batch = state.pending
			state.pending = nil
		}
		p.sessionMu.Unlock()
	}
	return firstErr
}

func (p *PawzoChatPlugin) sendOutput(receiver *contact.Contact, output outbound) error {
	if p.message == nil {
		return errors.New("message ability is not injected")
	}
	if receiver == nil || strings.TrimSpace(receiver.GetUsername()) == "" {
		return errors.New("receiver is empty")
	}
	msg := &message.Message{Receiver: receiver, Content: output.Text}
	switch output.Kind {
	case "text":
		msg.Type = message.TypeText
		msg.Data = &message.Message_Text{Text: &message.TextData{Content: output.Text}}
	case "image":
		msg.Type = message.TypeImage
		msg.Data = &message.Message_Image{Image: &message.ImageData{Media: mediaData(output.Data)}}
	case "emoji":
		msg.Type = message.TypeEmoji
		msg.Data = &message.Message_Emoji{Emoji: &message.EmojiData{
			Media: mediaData(output.Data), Desc: output.Text,
		}}
	case "voice":
		msg.Type = message.TypeVoice
		msg.Data = &message.Message_Voice{Voice: &message.VoiceData{
			Media: mediaData(output.Data), Duration: output.DurationMS,
		}}
	default:
		return errors.New("unsupported PawzoChat output kind: " + output.Kind)
	}
	_, err := p.message.Send(msg)
	return err
}

func mediaData(data []byte) *message.Media {
	return &message.Media{Data: data, Size: uint32(len(data))}
}

const identityRefreshTTL = 30 * time.Second

func (p *PawzoChatPlugin) refreshIdentity() {
	if p.contact == nil {
		return
	}
	p.identityRefresh.Lock()
	defer p.identityRefresh.Unlock()
	p.identityMu.RLock()
	fresh := p.self != nil && time.Since(p.identityUpdatedAt) < identityRefreshTTL
	p.identityMu.RUnlock()
	if fresh {
		return
	}
	self := p.contact.GetSelf()
	owner := p.contact.GetOwner()
	p.identityMu.Lock()
	if self != nil {
		p.self = proto.Clone(self).(*contact.SelfInfo)
	}
	if owner != nil && strings.TrimSpace(owner.GetUsername()) != "" {
		p.ownerID = strings.TrimSpace(owner.GetUsername())
		p.ownerName = displayContact(owner)
	}
	if self != nil || p.ownerID != "" {
		p.identityUpdatedAt = time.Now()
	}
	p.identityMu.Unlock()
}

func (p *PawzoChatPlugin) identityForEvent() (*contact.SelfInfo, string, string) {
	p.identityMu.RLock()
	fresh := p.self != nil && time.Since(p.identityUpdatedAt) < identityRefreshTTL
	p.identityMu.RUnlock()
	if !fresh {
		p.refreshIdentity()
	}
	p.identityMu.RLock()
	defer p.identityMu.RUnlock()
	if p.self == nil {
		return nil, p.ownerID, p.ownerName
	}
	return proto.Clone(p.self).(*contact.SelfInfo), p.ownerID, p.ownerName
}
