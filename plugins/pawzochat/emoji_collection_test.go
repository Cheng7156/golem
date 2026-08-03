package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
)

type emojiDownloadCDN struct {
	data   []byte
	fileID string
	aesKey string
	calls  int
}

func (d *emojiDownloadCDN) DownloadImage(fileID, aesKey string) (io.ReadCloser, error) {
	d.calls++
	d.fileID = fileID
	d.aesKey = aesKey
	return io.NopCloser(bytes.NewReader(d.data)), nil
}

type emojiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f emojiRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type blockingEmojiCDN struct {
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
}

type contextualEmojiCDN struct {
	fileID      string
	aesKey      string
	legacyCalls int
}

func (d *contextualEmojiCDN) DownloadImage(string, string) (io.ReadCloser, error) {
	d.legacyCalls++
	return nil, errors.New("legacy download should not be called")
}

func (d *contextualEmojiCDN) DownloadImageContext(
	ctx context.Context,
	fileID string,
	aesKey string,
) (io.ReadCloser, error) {
	d.fileID = fileID
	d.aesKey = aesKey
	<-ctx.Done()
	return nil, ctx.Err()
}

func (d *blockingEmojiCDN) DownloadImage(string, string) (io.ReadCloser, error) {
	close(d.started)
	<-d.release
	return &trackedEmojiReader{Reader: strings.NewReader("late"), closed: d.closed}, nil
}

type trackedEmojiReader struct {
	io.Reader
	closed chan struct{}
}

func (r *trackedEmojiReader) Close() error {
	close(r.closed)
	return nil
}

func testEmojiPNG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 12, 8))
	for y := range 8 {
		for x := range 12 {
			img.Set(x, y, color.RGBA{R: 230, G: 50, B: 90, A: 255})
		}
	}
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func emojiEvent(raw []byte) *plugin.Event {
	return &plugin.Event{Payload: &plugin.Event_Message{Message: &message.Message{
		Type: message.TypeEmoji,
		Sender: &contact.Contact{
			Username: "wxid_friend", Nickname: "Friend",
			Type: contact.ContactType_CONTACT_TYPE_FRIEND,
		},
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{Media: &message.Media{
			Md5: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Data: raw,
		}}},
	}}}
}

func TestEmojiCollectionChecksBeforeDownloadAndDoesNotConsumeEvent(t *testing.T) {
	checked := make(chan struct{}, 1)
	uploads := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/bridge/golem/emoji-candidates/check":
			checked <- struct{}{}
			_, _ = io.WriteString(w, `{"outcome":"disabled"}`)
		case "/api/bridge/golem/emoji-candidates/upload":
			uploads <- struct{}{}
			_, _ = io.WriteString(w, `{"outcome":"pending"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pawzo := newPawzoChatPlugin()
	pawzo.self = &contact.SelfInfo{Username: "wxid_bot", Nickname: "Bot"}
	pawzo.Config = normalizeConfigValue(Config{
		BaseURL: server.URL, HTTPTimeoutSeconds: 2,
		Routes: map[string]string{"private:wxid_friend": "persona"},
	})
	if err := pawzo.OnLoad(); err != nil {
		t.Fatal(err)
	}
	defer pawzo.OnUnload()

	handled, err := pawzo.OnEvent(emojiEvent(testEmojiPNG(t)))
	if err != nil || handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("emoji candidate was not checked")
	}
	select {
	case <-uploads:
		t.Fatal("disabled candidate was uploaded")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEmojiCollectionUploadsVerifiedBytesAsynchronously(t *testing.T) {
	type checkPayload struct {
		PersonaID  string `json:"persona_id"`
		SessionKey string `json:"session_key"`
		SenderID   string `json:"sender_id"`
	}
	uploaded := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/bridge/golem/emoji-candidates/check":
			var payload checkPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode check: %v", err)
			}
			if payload.PersonaID != "persona" || payload.SessionKey != "private:wxid_friend" || payload.SenderID != "wxid_friend" {
				t.Errorf("payload=%#v", payload)
			}
			_, _ = io.WriteString(w, `{"outcome":"upload_required"}`)
		case "/api/bridge/golem/emoji-candidates/upload":
			if err := r.ParseMultipartForm(maxCollectedEmojiBytes + 1024); err != nil {
				t.Errorf("multipart: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			file, _, err := r.FormFile("emoji")
			if err != nil {
				t.Errorf("form file: %v", err)
				http.Error(w, "missing file", http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(file)
			_ = file.Close()
			uploaded <- data
			_, _ = io.WriteString(w, `{"outcome":"pending"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	raw := testEmojiPNG(t)
	pawzo := newPawzoChatPlugin()
	pawzo.self = &contact.SelfInfo{Username: "wxid_bot", Nickname: "Bot"}
	pawzo.Config = normalizeConfigValue(Config{
		BaseURL: server.URL, DefaultPersonaID: "persona", HTTPTimeoutSeconds: 2,
	})
	if err := pawzo.OnLoad(); err != nil {
		t.Fatal(err)
	}
	defer pawzo.OnUnload()

	handled, err := pawzo.OnEvent(emojiEvent(raw))
	if err != nil || handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	select {
	case result := <-uploaded:
		if !bytes.Equal(raw, result) {
			t.Fatal("uploaded emoji bytes changed")
		}
	case <-time.After(time.Second):
		t.Fatal("emoji candidate was not uploaded")
	}
}

func TestEmojiValidationRejectsHTMLAndURLAllowlistRejectsUntrustedHost(t *testing.T) {
	if _, _, err := validateEmojiBytes([]byte("<html>not an image</html>")); err == nil {
		t.Fatal("HTML was accepted as an emoji")
	}
	parsed, err := url.Parse("https://example.com/sticker.gif")
	if err != nil {
		t.Fatal(err)
	}
	if allowedEmojiCDNURL(parsed) {
		t.Fatal("untrusted CDN host was accepted")
	}
	trusted, _ := url.Parse("https://vweixinf.tc.qq.com/sticker.gif")
	if !allowedEmojiCDNURL(trusted) {
		t.Fatal("WeChat CDN host was rejected")
	}
}

func TestEmojiDownloadPrefersTrustedDirectURL(t *testing.T) {
	raw := testEmojiPNG(t)
	cdn := &emojiDownloadCDN{data: raw}
	client := &http.Client{Transport: emojiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() != "vweixinf.tc.qq.com" {
			t.Fatalf("unexpected host: %s", request.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    request,
		}, nil
	})}
	job := emojiCollectionJob{
		WeChatMD5: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FileID:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AESKey:    "aes-key",
		CDNURL:    "https://vweixinf.tc.qq.com/sticker?token=secret",
	}

	downloaded, contentType, extension, err := resolveEmojiDownload(job, cdn, client)
	if err != nil {
		t.Fatalf("download emoji: %v", err)
	}
	if !bytes.Equal(downloaded, raw) || contentType != "image/png" || extension != ".png" {
		t.Fatalf("unexpected result: content=%q extension=%q bytes=%d", contentType, extension, len(downloaded))
	}
	if cdn.calls != 0 {
		t.Fatalf("direct URL unexpectedly reached CDN: %d calls", cdn.calls)
	}
}

func TestEmojiDownloadFallsBackToCDNWithMD5FileID(t *testing.T) {
	raw := testEmojiPNG(t)
	cdn := &emojiDownloadCDN{data: raw}
	client := &http.Client{Transport: emojiRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("direct URL unavailable")
	})}
	const md5 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	job := emojiCollectionJob{
		WeChatMD5: md5,
		FileID:    md5,
		AESKey:    "aes-key",
		CDNURL:    "https://vweixinf.tc.qq.com/sticker?token=secret",
	}

	downloaded, _, _, err := resolveEmojiDownload(job, cdn, client)
	if err != nil {
		t.Fatalf("download emoji: %v", err)
	}
	if !bytes.Equal(downloaded, raw) {
		t.Fatal("CDN bytes changed")
	}
	if cdn.calls != 1 || cdn.fileID != md5 || cdn.aesKey != "aes-key" {
		t.Fatalf("unexpected CDN request: calls=%d file_id=%q aes_key=%q", cdn.calls, cdn.fileID, cdn.aesKey)
	}
}

func TestEmojiURLDownloadReturnsAtDeadline(t *testing.T) {
	client := &http.Client{Transport: emojiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	startedAt := time.Now()
	_, _, _, err := downloadEmojiURL(
		"https://vweixinf.tc.qq.com/sticker?token=secret",
		20*time.Millisecond,
		client,
	)
	if err == nil || err.Error() != "emoji CDN request timed out" {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("direct URL timeout took %s", elapsed)
	}
}

func TestContextualEmojiCDNDownloadUsesCancellableMethod(t *testing.T) {
	cdn := &contextualEmojiCDN{}
	_, err := downloadEmojiCDN(context.Background(), cdn, "file-id", "aes-key", 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
	if cdn.legacyCalls != 0 || cdn.fileID != "file-id" || cdn.aesKey != "aes-key" {
		t.Fatalf("unexpected CDN call: legacy=%d file_id=%q aes_key=%q", cdn.legacyCalls, cdn.fileID, cdn.aesKey)
	}
}

func TestLegacyEmojiCDNDownloadReturnsAtDeadline(t *testing.T) {
	cdn := &blockingEmojiCDN{
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	startedAt := time.Now()
	_, err := downloadEmojiCDN(context.Background(), cdn, "file-id", "aes-key", 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("legacy CDN timeout took %s", elapsed)
	}
	select {
	case <-cdn.started:
	default:
		t.Fatal("legacy CDN was not called")
	}
	close(cdn.release)
	select {
	case <-cdn.closed:
	case <-time.After(time.Second):
		t.Fatal("late CDN reader was not closed")
	}
}

func TestEmojiValidationAcceptsAnimatedWebPContainer(t *testing.T) {
	chunk := func(name string, payload []byte) []byte {
		result := make([]byte, 8+len(payload))
		copy(result, name)
		binary.LittleEndian.PutUint32(result[4:8], uint32(len(payload)))
		copy(result[8:], payload)
		return result
	}
	// VP8X animation canvas 12x8 plus an ANIM/ANMF frame marker. The Python
	// service performs the definitive full-image verification before storage.
	vp8x := []byte{0x02, 0, 0, 0, 11, 0, 0, 7, 0, 0}
	riff := append([]byte("RIFF\x00\x00\x00\x00WEBP"), chunk("VP8X", vp8x)...)
	riff = append(riff, chunk("ANIM", make([]byte, 6))...)
	riff = append(riff, chunk("ANMF", make([]byte, 16))...)
	binary.LittleEndian.PutUint32(riff[4:8], uint32(len(riff)-8))

	contentType, extension, err := validateEmojiBytes(riff)
	if err != nil {
		t.Fatalf("validate animated WebP: %v", err)
	}
	if contentType != "image/webp" || extension != ".webp" {
		t.Fatalf("content=%q extension=%q", contentType, extension)
	}
}

func TestSubscriptionsIncludeEmoji(t *testing.T) {
	subscriptions := newPawzoChatPlugin().GetSubscriptions()
	if !slices.Contains(subscriptions, message.TypeEmoji.Topic) {
		t.Fatalf("subscriptions=%s", strings.Join(subscriptions, ","))
	}
}
