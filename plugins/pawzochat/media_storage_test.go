package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
)

func TestImageEventStoresMediaWithoutCallingMessageBridge(t *testing.T) {
	stored := make(chan struct{}, 1)
	var messageCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/bridge/golem/media":
			if err := r.ParseMultipartForm(maxCollectedEmojiBytes + 1024); err != nil {
				t.Errorf("ParseMultipartForm: %v", err)
			}
			if r.FormValue("session_key") != "private:wxid_friend" {
				t.Errorf("session_key=%q", r.FormValue("session_key"))
			}
			if !strings.Contains(r.FormValue("context"), `"text":"[图片]"`) {
				t.Errorf("context=%q", r.FormValue("context"))
			}
			file, _, err := r.FormFile("image")
			if err != nil {
				t.Errorf("FormFile: %v", err)
			} else {
				_ = file.Close()
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"outcome":"stored","image_id":"img_0123456789abcdef01234567"}`))
			stored <- struct{}{}
		case "/api/bridge/golem/messages":
			messageCalls.Add(1)
			_, _ = w.Write([]byte(`{"outcome":"no_reply","messages":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pawzo := newPawzoChatPlugin()
	pawzo.self = &contact.SelfInfo{Username: "wxid_self", Nickname: "Bot"}
	pawzo.ownerID = "wxid_owner"
	pawzo.Config = normalizeConfigValue(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 2,
		Routes: map[string]string{"private:wxid_friend": "persona"},
	})
	pawzo.startMediaWorkers()
	defer pawzo.stopMediaWorkers()
	event := &plugin.Event{Payload: &plugin.Event_Message{Message: &message.Message{
		Type: message.TypeImage,
		Sender: &contact.Contact{
			Username: "wxid_friend", Nickname: "Friend",
			Type: contact.ContactType_CONTACT_TYPE_FRIEND,
		},
		Data: &message.Message_Image{Image: &message.ImageData{
			Media: &message.Media{Data: testEmojiPNG(t)},
		}},
	}}}

	handled, err := pawzo.OnEvent(event)
	if err != nil || handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	<-stored
	if messageCalls.Load() != 0 {
		t.Fatalf("message bridge calls=%d", messageCalls.Load())
	}
}

func TestFollowingTextWaitsForMediaCompletion(t *testing.T) {
	pawzo := newPawzoChatPlugin()
	done := make(chan struct{})
	pawzo.mediaPending = map[string]chan struct{}{
		"private:wxid_friend": done,
	}
	pawzo.mediaStop = make(chan struct{})
	returned := make(chan struct{})

	go func() {
		pawzo.waitForPendingMedia("private:wxid_friend")
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("media wait returned before the upload completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("media wait did not return after the upload completed")
	}
}
