package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"testing"

	"github.com/sbgayhub/golem/sdk/message"
	"golem_plugin_hermes/internal/domain"
	"google.golang.org/protobuf/proto"
)

type downloadMessageAbility struct {
	data       []byte
	downloaded *message.Message
	err        error
}

type downloadCDNAbility struct {
	data   []byte
	fileID string
	aesKey string
	calls  int
	err    error
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

func (a *downloadCDNAbility) DownloadImage(fileID, aesKey string) (io.ReadCloser, error) {
	a.calls++
	a.fileID = fileID
	a.aesKey = aesKey
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

func TestGolemMediaResolverDownloadsImageViaCDN(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	source := &message.Message{
		Id: 46,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{
			Md5: "cdn-file-id", Key: "cdn-aes-key", Url: "https://encrypted.example/image",
		}}},
	}
	media, err := inboundMedia(source)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	cdn := &downloadCDNAbility{data: pngData}
	messageAbility := &downloadMessageAbility{err: errors.New("DownloadImg not supported in lib mode, use web mode")}
	resolved, err := (golemMediaResolver{message: messageAbility, cdn: cdn}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cdn.calls != 1 || cdn.fileID != "cdn-file-id" || cdn.aesKey != "cdn-aes-key" {
		t.Fatalf("unexpected CDN request: calls=%d file_id=%q aes_key=%q", cdn.calls, cdn.fileID, cdn.aesKey)
	}
	if messageAbility.downloaded != nil {
		t.Fatal("message downloader was used before the lib-mode CDN path")
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) || resolved[0].MIMEType != "image/png" {
		t.Fatalf("resolved media=%#v", resolved)
	}
}

func TestInboundMediaRecoversRawWechatImageThumbnail(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	msg := &message.Message{
		Id:   48,
		Type: message.TypeImage,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{}}},
		Raw: rawWechatImageJSON(t,
			"member:\n<?xml version=\"1.0\"?><msg><imgmsg md5=\"raw-md5\" aeskey=\"raw-key\" cdnmidimgurl=\"raw-file-id\" length=\"123\"/></msg>",
			pngData,
		),
	}
	media, err := inboundMedia(msg)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || !bytes.Equal(media[0].Data, pngData) || media[0].MIMEType != "image/png" {
		t.Fatalf("recovered media=%#v", media)
	}
	if media[0].MD5 != "raw-md5" || media[0].URL != "" || len(media[0].DownloadSource) != 0 {
		t.Fatalf("recovered image metadata=%#v", media[0])
	}
}

func TestGolemMediaResolverRecoversRawWechatImageCDNIdentifiers(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	msg := &message.Message{
		Id:   49,
		Type: message.TypeImage,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{}}},
		Raw: rawWechatImageJSON(t,
			"room-member:\n<msg><imgmsg md5=\"raw-md5\" aeskey=\"raw-key\" cdnmidimgurl=\"raw-file-id\" length=\"456\"/></msg>",
			nil,
		),
	}
	media, err := inboundMedia(msg)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || len(media[0].DownloadSource) == 0 {
		t.Fatalf("deferred media=%#v", media)
	}
	cdn := &downloadCDNAbility{data: pngData}
	resolved, err := (golemMediaResolver{cdn: cdn}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cdn.fileID != "raw-file-id" || cdn.aesKey != "raw-key" {
		t.Fatalf("recovered CDN request: file_id=%q aes_key=%q", cdn.fileID, cdn.aesKey)
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) {
		t.Fatalf("resolved media=%#v", resolved)
	}
}

func TestGolemMediaResolverRecoversRawWechatImageMD5WhenHostMediaIsEmpty(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	const md5 = "0123456789abcdef0123456789abcdef"
	msg := &message.Message{
		Id:   490,
		Type: message.TypeImage,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{}}},
		Raw: rawWechatImageJSON(t,
			`<msg><imgmsg md5="`+md5+`" aeskey="raw-key" cdnmidimgurl="opaque-cdn-id" length="456"/></msg>`,
			nil,
		),
	}
	source, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal persisted source: %v", err)
	}
	// This is the shape persisted by older Golem versions: the outer media
	// metadata is empty, while the raw Message still carries <imgmsg> fields.
	media := []domain.InboundMedia{{Kind: "image", DownloadSource: source}}
	cdn := &downloadCDNAbility{data: pngData}
	resolved, err := (golemMediaResolver{cdn: cdn}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cdn.fileID != md5 || cdn.aesKey != "raw-key" {
		t.Fatalf("recovered raw MD5 was not used: file_id=%q aes_key=%q", cdn.fileID, cdn.aesKey)
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) {
		t.Fatalf("resolved media=%#v", resolved)
	}
}

func TestGolemMediaResolverDownloadsEmojiViaCDN(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	source := &message.Message{
		Id:   47,
		Type: message.TypeEmoji,
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{Desc: "wave", Media: &message.Media{
			Md5: "sticker-file-id", Key: "sticker-aes-key", Url: "https://untrusted.example/sticker.gif",
		}}},
	}
	media, err := inboundMedia(source)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || len(media[0].DownloadSource) == 0 || media[0].URL != "" {
		t.Fatalf("emoji was not safely deferred: %#v", media)
	}
	cdn := &downloadCDNAbility{data: pngData}
	resolved, err := (golemMediaResolver{cdn: cdn}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cdn.calls != 1 || cdn.fileID != "sticker-file-id" || cdn.aesKey != "sticker-aes-key" {
		t.Fatalf("unexpected CDN request: calls=%d file_id=%q aes_key=%q", cdn.calls, cdn.fileID, cdn.aesKey)
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) || resolved[0].Kind != "emoji" {
		t.Fatalf("resolved emoji=%#v", resolved)
	}
}

func TestGolemMediaResolverUsesRawWechatURLBeforeCDN(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	const mediaURL = "http://vweixinf.tc.qq.com/110/20401/stodownload?m=raw-md5&filekey=raw-file-key"
	msg := &message.Message{
		Id:   53,
		Type: message.TypeEmoji,
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{Media: &message.Media{}}},
		Raw: rawWechatImageJSON(t,
			`<msg><emoji md5="raw-md5" aeskey="raw-key" cdnurl="http://vweixinf.tc.qq.com/110/20401/stodownload?m=raw-md5&amp;filekey=raw-file-key"/></msg>`,
			nil,
		),
	}
	media, err := inboundMedia(msg)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || media[0].URL != "" || len(media[0].DownloadSource) == 0 {
		t.Fatalf("emoji should remain deferred until explicit visual request: %#v", media)
	}
	cdn := &downloadCDNAbility{err: errors.New("CDN unavailable")}
	httpClient := &http.Client{Transport: mediaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != mediaURL {
			t.Fatalf("unexpected fallback URL: %s", request.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(pngData)),
			Request:    request,
		}, nil
	})}
	resolved, err := (golemMediaResolver{cdn: cdn, httpClient: httpClient}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cdn.calls != 0 {
		t.Fatalf("direct URL unexpectedly reached CDN: %d calls", cdn.calls)
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) || resolved[0].MIMEType != "image/png" {
		t.Fatalf("resolved media=%#v", resolved)
	}
}

func TestGolemMediaResolverConvertsGIFToPNGForVision(t *testing.T) {
	var source bytes.Buffer
	frame := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{
		color.Transparent,
		color.RGBA{R: 255, A: 255},
	})
	frame.SetColorIndex(0, 0, 1)
	if err := gif.Encode(&source, frame, nil); err != nil {
		t.Fatalf("encode GIF: %v", err)
	}
	resolved, err := (golemMediaResolver{}).Resolve(context.Background(), []domain.InboundMedia{{
		Kind: "emoji", Data: source.Bytes(), MIMEType: "image/gif",
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].MIMEType != "image/png" {
		t.Fatalf("resolved GIF metadata=%#v", resolved)
	}
	if got := http.DetectContentType(resolved[0].Data); got != "image/png" {
		t.Fatalf("converted GIF content type=%q", got)
	}
	if _, err := png.Decode(bytes.NewReader(resolved[0].Data)); err != nil {
		t.Fatalf("converted GIF is not PNG: %v", err)
	}
}

type mediaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mediaRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestInboundMediaKeepsAllowlistedWechatEmojiURL(t *testing.T) {
	const mediaURL = "http://wxapp.tc.qq.com/262/20304/stodownload?m=trusted-md5"
	msg := &message.Message{
		Id:   50,
		Type: message.TypeEmoji,
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{Media: &message.Media{
			Md5: "trusted-md5", Url: mediaURL,
		}}},
	}
	media, err := inboundMedia(msg)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || media[0].URL != mediaURL || len(media[0].DownloadSource) != 0 {
		t.Fatalf("trusted emoji media=%#v", media)
	}
}

func TestInboundMediaPrefersAllowlistedURLBeforeCDN(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	const mediaURL = "https://wxapp.tc.qq.com/262/20304/stodownload?m=trusted-md5"
	msg := &message.Message{
		Id:   52,
		Type: message.TypeEmoji,
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{Media: &message.Media{
			Md5: "trusted-md5", Key: "sticker-key", Url: mediaURL,
		}}},
		Raw: rawWechatImageJSON(t,
			`<msg><emoji md5="trusted-md5" aeskey="sticker-key" cdnmidimgurl="opaque-sticker-id" url="`+mediaURL+`"/></msg>`,
			nil,
		),
	}
	media, err := inboundMedia(msg)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	if len(media) != 1 || media[0].URL != "" || len(media[0].DownloadSource) == 0 {
		t.Fatalf("encrypted URL bypassed CDN materialization: %#v", media)
	}
	cdn := &downloadCDNAbility{data: pngData}
	httpClient := &http.Client{Transport: mediaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != mediaURL {
			t.Fatalf("unexpected direct URL: %s", request.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(pngData)), Request: request,
		}, nil
	})}
	resolved, err := (golemMediaResolver{cdn: cdn, httpClient: httpClient}).Resolve(context.Background(), media)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cdn.calls != 0 {
		t.Fatalf("allowlisted direct URL unexpectedly reached CDN: %d calls", cdn.calls)
	}
	if len(resolved) != 1 || !bytes.Equal(resolved[0].Data, pngData) {
		t.Fatalf("resolved media=%#v", resolved)
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

func TestGolemMediaResolverReturnsCDNFailureDespiteUnsupportedMessageFallback(t *testing.T) {
	source := &message.Message{
		Id: 51,
		Data: &message.Message_Image{Image: &message.ImageData{Media: &message.Media{
			Md5: "cdn-file-id", Key: "cdn-key",
		}}},
	}
	media, err := inboundMedia(source)
	if err != nil {
		t.Fatalf("inboundMedia: %v", err)
	}
	cdn := &downloadCDNAbility{err: errors.New("CDN EOF")}
	messageAbility := &downloadMessageAbility{err: errors.New("DownloadImg not supported in lib mode, use web mode")}
	if _, err := (golemMediaResolver{message: messageAbility, cdn: cdn}).Resolve(context.Background(), media); err == nil {
		t.Fatal("expected the CDN failure to remain retryable")
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

func rawWechatImageJSON(t *testing.T, content string, data []byte) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"content":      map[string]any{"value": content},
		"image_buffer": map[string]any{"data": data},
	})
	if err != nil {
		t.Fatalf("marshal raw WeChat image: %v", err)
	}
	return string(raw)
}
