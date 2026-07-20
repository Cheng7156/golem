package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/output"

	"github.com/sbgayhub/golem/sdk/message"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReceiptRequiresNonZeroMessageID(t *testing.T) {
	for _, response := range []*message.Send_Response{nil, {}} {
		_, err := receipt(response)
		var ambiguous output.AmbiguousError
		if !errors.As(err, &ambiguous) {
			t.Fatalf("response=%#v error=%v, want AmbiguousError", response, err)
		}
	}
	value, err := receipt(&message.Send_Response{NewId: 42})
	if err != nil || value.ID != 42 {
		t.Fatalf("receipt=%#v err=%v", value, err)
	}
}

func TestClassifySendErrorPreservesPermanentHostCodes(t *testing.T) {
	err := classifySendError(status.Error(codes.FailedPrecondition, "rejected"), true)
	var permanent output.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("error=%v, want PermanentError", err)
	}
	err = classifySendError(status.Error(codes.Unavailable, "down"), true)
	var ambiguous output.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error=%v, want AmbiguousError", err)
	}
}

type blockingMessageAbility struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type senderMediaReader struct {
	data    map[string][]byte
	objects map[string]domain.MediaObject
}

func (r senderMediaReader) Read(
	_ context.Context,
	objectID string,
	expectedKind string,
) ([]byte, domain.MediaObject, error) {
	object := r.objects[objectID]
	if object.Kind != expectedKind {
		return nil, domain.MediaObject{}, io.ErrUnexpectedEOF
	}
	return append([]byte(nil), r.data[objectID]...), object, nil
}

func (a *blockingMessageAbility) Send(*message.Message) (*message.Send_Response, error) {
	a.calls.Add(1)
	a.started <- struct{}{}
	<-a.release
	return &message.Send_Response{}, nil
}

func (*blockingMessageAbility) Forward(*message.Message, string) (*message.Forward_Response, error) {
	return &message.Forward_Response{}, nil
}

func (*blockingMessageAbility) Revoke(string, uint64) (*message.Revoke_Response, error) {
	return &message.Revoke_Response{}, nil
}

func (*blockingMessageAbility) Download(*message.Message) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

func TestGolemSenderBuildsTextImageAndEmojiMessages(t *testing.T) {
	sender := newGolemSender(nil, 1, nil)
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	tests := []struct {
		name    string
		kind    string
		payload any
		code    int32
	}{
		{name: "text", kind: "text", payload: domain.TextOutput{Content: "hello"}, code: message.TypeText.Code},
		{name: "image", kind: "image", payload: domain.ImageOutput{Data: pngData, Alt: "image"}, code: message.TypeImage.Code},
		{name: "emoji", kind: "emoji", payload: domain.EmojiOutput{Data: pngData, Description: "emoji"}, code: message.TypeEmoji.Code},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			msg, err := sender.outboxMessage(context.Background(), domain.OutboxItem{
				ReceiverID: "receiver", Kind: test.kind, Payload: payload,
			})
			if err != nil {
				t.Fatalf("outboxMessage: %v", err)
			}
			if msg.GetType().GetCode() != test.code || msg.GetReceiver().GetUsername() != "receiver" {
				t.Fatalf("unexpected message: %#v", msg)
			}
			if test.kind != "text" {
				assertHostUploadMedia(t, msg, pngData)
			}
		})
	}
}

func assertHostUploadMedia(t *testing.T, msg *message.Message, expected []byte) {
	t.Helper()
	var media *message.Media
	switch msg.GetType().GetCode() {
	case message.TypeImage.Code:
		media = msg.GetImage().GetMedia()
	case message.TypeEmoji.Code:
		media = msg.GetEmoji().GetMedia()
	default:
		t.Fatalf("unexpected media message type=%d", msg.GetType().GetCode())
	}
	if media == nil || !bytes.Equal(media.GetData(), expected) {
		t.Fatalf("message.Send did not carry original media bytes: %#v", media)
	}
	if media.GetUrl() != "" || media.GetKey() != "" || media.GetSize() != uint32(len(expected)) {
		t.Fatalf("sender attempted pre-uploaded CDN metadata: %#v", media)
	}
}

func TestPublicIPRejectsPrivateAndLoopbackNetworks(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "::1"} {
		if publicIP(net.ParseIP(value)) {
			t.Fatalf("publicIP(%s)=true", value)
		}
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public IPv4 address was rejected")
	}
}

func TestGolemSenderBuildsVideoFromMediaObjects(t *testing.T) {
	videoData := append([]byte{0, 0, 0, 24}, []byte("ftypisom-video")...)
	thumbData := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1, 1, 0}
	reader := senderMediaReader{
		data: map[string][]byte{"video": videoData, "thumb": thumbData},
		objects: map[string]domain.MediaObject{
			"video": {ID: "video", Kind: domain.MediaKindVideo, MIMEType: "video/mp4"},
			"thumb": {ID: "thumb", Kind: domain.MediaKindThumbnail, MIMEType: "image/jpeg"},
		},
	}
	sender := newGolemSender(nil, 1, reader)
	payload, _ := json.Marshal(domain.VideoOutput{
		ObjectID: "video", ThumbObjectID: "thumb", Duration: 4, Title: "sample",
	})
	msg, err := sender.outboxMessage(context.Background(), domain.OutboxItem{
		ReceiverID: "receiver", Kind: "video", Payload: payload,
	})
	if err != nil {
		t.Fatalf("outboxMessage: %v", err)
	}
	video := msg.GetVideo()
	if msg.GetType().GetCode() != message.TypeVideo.Code || video.GetDuration() != 4 {
		t.Fatalf("message=%#v", msg)
	}
	if !bytes.Equal(video.GetMedia().GetData(), videoData) || !bytes.Equal(video.GetThumb(), thumbData) {
		t.Fatalf("video data was not carried by message.Send: %#v", video)
	}
}

func TestFallbackSenderKeepsSlotUntilUnderlyingCallReturns(t *testing.T) {
	ability := &blockingMessageAbility{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	defer close(ability.release)
	sender := newGolemSender(ability, 1, nil)
	payload, err := json.Marshal(domain.TextOutput{Content: "hello"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	item := domain.OutboxItem{ReceiverID: "receiver", Kind: "text", Payload: payload}

	firstContext, cancelFirst := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, err := sender.Send(firstContext, item)
		firstDone <- err
	}()
	select {
	case <-ability.started:
	case <-time.After(time.Second):
		t.Fatal("first SDK send did not start")
	}
	if err := <-firstDone; err == nil {
		t.Fatal("timed out SDK send returned success")
	}
	if len(sender.fallbackSlots) != 1 {
		t.Fatalf("active fallback slots=%d, want 1", len(sender.fallbackSlots))
	}

	secondContext, cancelSecond := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelSecond()
	if _, err := sender.Send(secondContext, item); err == nil {
		t.Fatal("second send bypassed the occupied fallback slot")
	}
	if ability.calls.Load() != 1 {
		t.Fatalf("underlying Send calls=%d, want 1", ability.calls.Load())
	}
}
