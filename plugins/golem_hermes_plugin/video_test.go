package main

import (
	"bytes"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sbgayhub/golem/sdk/message"
)

// TestOutflowSendVideo 发送层：合法视频走 TypeVideo，超限拒绝
func TestOutflowSendVideo(t *testing.T) {
	p, fake := newFunTestPlugin(t)
	if err := p.out.sendVideo("g@chatroom", bytes.Repeat([]byte{0x01}, 64*1024), placeholderThumb(), 15); err != nil {
		t.Fatalf("合法视频应发送成功: %v", err)
	}
	if len(fake.sends) != 1 || fake.sends[0].GetType().Code != message.TypeVideo.Code {
		t.Fatalf("应发出一条 TypeVideo 消息, got %v", fake.sends)
	}
	// 超限
	if err := p.out.sendVideo("g@chatroom", make([]byte, maxVideoBytes+1), nil, 15); err == nil {
		t.Fatal("超限视频应被拒绝")
	}
	if len(fake.sends) != 1 {
		t.Fatal("超限视频不应发送")
	}
}

// newVideoTestBridge 构造带活跃会话的 bridge + 视频配置
func newVideoTestBridge(t *testing.T) (*bridgeServer, *fakeAbility) {
	p, fake := newFunTestPlugin(t)
	p.Config.VideoDailyCap = 2
	p.Config.VideoMaxBytes = 1024 * 1024 // 1MB 方便测超限
	p.Config.VideoTimeoutSec = 5
	p.rings = newRingStore(60)
	p.disp = newDispatcher(&p.Config, nil, p.out, p.rings, p.store, p.metrics)
	b := &bridgeServer{p: p}
	// 播种活跃会话（工具桥防越权的绑定前提）
	p.disp.mu.Lock()
	p.disp.active["wx:group:g@chatroom"] = &activeSession{
		Receiver: "g@chatroom", IsGroup: true, GroupID: "g@chatroom",
		LastActive: time.Now(),
		LastTask:   mkTask("g@chatroom", "mention", "发个视频"),
	}
	p.disp.mu.Unlock()
	return b, fake
}

func videoReq(b *bridgeServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/video", strings.NewReader(body))
	req.Header.Set("X-Hermes-Session-Key", "wx:group:g@chatroom")
	rw := httptest.NewRecorder()
	b.handleVideo(rw, req)
	return rw
}

func TestHandleVideoE2E(t *testing.T) {
	b, fake := newVideoTestBridge(t)
	// 模拟视频源：800KB video/mp4
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(bytes.Repeat([]byte{0x66}, 800*1024))
	}))
	defer src.Close()

	rw := videoReq(b, `{"url":"`+src.URL+`/clip.mp4","duration":12}`)
	if rw.Code != 200 {
		t.Fatalf("应发送成功: %d %s", rw.Code, rw.Body.String())
	}
	if len(fake.sends) != 1 || fake.sends[0].GetType().Code != message.TypeVideo.Code {
		t.Fatal("应发出 TypeVideo")
	}
	if got := fake.sends[0].GetVideo().GetDuration(); got != 12 {
		t.Fatalf("duration 应为 12, got %d", got)
	}
	if len(fake.sends[0].GetVideo().GetThumb()) < 100 {
		t.Fatal("占位缩略图缺失")
	}
	// 日配额记账
	if kv := b.p.store.kvGet("vidcap:g@chatroom:" + time.Now().Format("20060102")); kv != "1" {
		t.Fatalf("日配额 kv 应为 1, got %q", kv)
	}
}

func TestHandleVideoGates(t *testing.T) {
	b, fake := newVideoTestBridge(t)

	// 非 http(s) scheme
	if rw := videoReq(b, `{"url":"ftp://x.com/a.mp4"}`); rw.Code != 400 {
		t.Fatalf("ftp 应 400, got %d", rw.Code)
	}
	// 超过上限（源 2MB > 配置 1MB）
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(bytes.Repeat([]byte{0x66}, 2*1024*1024))
	}))
	defer big.Close()
	if rw := videoReq(b, `{"url":"` + big.URL + `/big.mp4"}`); rw.Code != 413 {
		t.Fatalf("超限应 413, got %d %s", rw.Code, rw.Body.String())
	}
	// 内容类型不是视频
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, 100*1024))
	}))
	defer bad.Close()
	if rw := videoReq(b, `{"url":"` + bad.URL + `/page.html"}`); rw.Code != 415 {
		t.Fatalf("非视频类型应 415, got %d", rw.Code)
	}
	// 日配额用尽（cap=2，先写满）
	b.p.store.kvSet("vidcap:g@chatroom:"+time.Now().Format("20060102"), "2")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(bytes.Repeat([]byte{0x66}, 800*1024))
	}))
	defer ok.Close()
	if rw := videoReq(b, `{"url":"` + ok.URL + `/c.mp4"}`); rw.Code != 429 {
		t.Fatalf("配额用尽应 429, got %d", rw.Code)
	}
	if len(fake.sends) != 0 {
		t.Fatalf("闸门全拒后不应有发送, got %d", len(fake.sends))
	}
}

func TestPlaceholderThumb(t *testing.T) {
	thumb := placeholderThumb()
	if len(thumb) < 100 {
		t.Fatalf("缩略图过小: %d", len(thumb))
	}
	if _, err := jpeg.Decode(bytes.NewReader(thumb)); err != nil {
		t.Fatalf("应为合法 JPEG: %v", err)
	}
}
