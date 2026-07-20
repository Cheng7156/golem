package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

func textOutboxMessage(receiver *contact.Contact, payloadJSON json.RawMessage) (*message.Message, error) {
	var payload domain.TextOutput
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("decode text outbox: %w", err)
	}
	payload.Content = strings.TrimSpace(payload.Content)
	if payload.Content == "" {
		return nil, errors.New("text outbox content is empty")
	}
	return &message.Message{
		Type: message.TypeText, Receiver: receiver, Content: payload.Content,
		Data: &message.Message_Text{Text: &message.TextData{Content: payload.Content}},
	}, nil
}

func imageOutboxMessage(
	ctx context.Context,
	receiver *contact.Contact,
	payloadJSON json.RawMessage,
) (*message.Message, error) {
	var payload domain.ImageOutput
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("decode image outbox: %w", err)
	}
	data, err := loadOutboundMedia(ctx, payload.URL, payload.Data)
	if err != nil {
		return nil, fmt.Errorf("load image outbox: %w", err)
	}
	if err := validateImage(data); err != nil {
		return nil, err
	}
	return &message.Message{
		Type: message.TypeImage, Receiver: receiver, Content: strings.TrimSpace(payload.Alt),
		Data: &message.Message_Image{Image: &message.ImageData{
			Media: &message.Media{Data: data, Size: uint32(len(data))},
		}},
	}, nil
}

func emojiOutboxMessage(
	ctx context.Context,
	receiver *contact.Contact,
	payloadJSON json.RawMessage,
) (*message.Message, error) {
	var payload domain.EmojiOutput
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("decode emoji outbox: %w", err)
	}
	data, err := loadOutboundMedia(ctx, payload.URL, payload.Data)
	if err != nil {
		return nil, fmt.Errorf("load emoji outbox: %w", err)
	}
	if err := validateImage(data); err != nil {
		return nil, err
	}
	mediaMD5 := strings.TrimSpace(payload.MD5)
	if mediaMD5 == "" {
		sum := md5.Sum(data)
		mediaMD5 = hex.EncodeToString(sum[:])
	}
	return &message.Message{
		Type: message.TypeEmoji, Receiver: receiver, Content: strings.TrimSpace(payload.Description),
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{
			Media: &message.Media{Data: data, Size: uint32(len(data)), Md5: mediaMD5},
			Desc:  strings.TrimSpace(payload.Description),
		}},
	}, nil
}
