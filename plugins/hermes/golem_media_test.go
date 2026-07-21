package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"testing"

	"github.com/sbgayhub/golem/sdk/message"
)

type downloadMessageAbility struct {
	data       []byte
	downloaded *message.Message
	err        error
}

func (*downloadMessageAbility) Send(*message.Message) (*message.Send_Response, error) {
	return &message.Send_Response{}, nil
}

func (*downloadMessageAbility) Forward(*message.Message, string) (*message.Forward_Response, error) {
	return &message.Forward_Response{}, nil
}

func (*downloadMessageAbility) Revoke(string, uint64) (*message.Revoke_Response, error) {
	return &message.Revoke_Response{}, nil
}

func (a *downloadMessageAbility) Download(value *message.Message) (io.ReadCloser, error) {
	a.downloaded = value
	if a.err != nil {
		return nil, a.err
	}
	return io.NopCloser(bytes.NewReader(a.data)), nil
}

func TestGolemMediaResolverDownloadsPersistedImageSource(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	source := &message.Message{
		Id: 42,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{
			Url: "https://encrypted.example/image",
			Key: "aes-key",
		}}},
	}
	media, err := inboundMedia(source)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || len(media[0].DownloadSource) == 0 || len(media[0].Data) != 0 {
		t.Fatalf("persisted media=%#v", media)
	}
	ability := &downloadMessageAbility{data: pngData}
	resolved, err := (golemMediaResolver{message: ability}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ability.downloaded == nil || ability.downloaded.GetId() != source.GetId() || ability.downloaded.GetImage().GetMedia().GetKey() != "aes-key" {
		t.Fatalf("download source was not restored: %#v", ability.downloaded)
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) || resolved[0].MIMEType != "image/png" {
		t.Fatalf("resolved media=%#v", resolved)
	}
	if len(resolved[0].DownloadSource) != 0 {
		t.Fatal("download source was retained after media resolution")
	}
}

func TestGolemMediaResolverKeepsLibModeImageAsObservableMetadata(t *testing.T) {
	source := &message.Message{
		Id: 43,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{
			Key: "aes-key",
		}}},
	}
	media, err := inboundMedia(source)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	ability := &downloadMessageAbility{err: errors.New("DownloadImg not supported in lib mode, use web mode")}
	resolved, err := (golemMediaResolver{message: ability}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || len(resolved[0].Data) != 0 || len(resolved[0].DownloadSource) != 0 {
		t.Fatalf("resolved media=%#v", resolved)
	}
}

func TestGolemMediaResolverReturnsOtherDownloadErrors(t *testing.T) {
	source := &message.Message{
		Id: 44,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{
			Key: "aes-key",
		}}},
	}
	media, err := inboundMedia(source)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	ability := &downloadMessageAbility{err: errors.New("network unavailable")}
	if _, err := (golemMediaResolver{message: ability}).Resolve(context.Background(), media); err == nil {
		t.Fatal("expected download error")
	}
}

func TestInboundMediaDoesNotTrustRemoteEmojiURL(t *testing.T) {
	msg := &message.Message{
		Id:   45,
		Type: message.TypeEmoji,
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{
			Desc: "wave",
			Media: &message.Media{
				Url: "https://remote.example/sticker.gif",
				Md5: "unverified-md5",
			},
		}},
	}
	media, err := inboundMedia(msg)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 0 {
		t.Fatalf("remote emoji URL became trusted inbound media: %#v", media)
	}
	if got := messageText(msg); got != "[sticker: wave]" {
		t.Fatalf("observable emoji placeholder=%q", got)
	}
}
