package main

import (
	"sync"
	"sync/atomic"

	"github.com/sbgayhub/golem/sdk/message"
)

// ringEntry 群上下文单条记录
type ringEntry struct {
	Seq     uint64 `json:"seq"`
	Speaker string `json:"speaker"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// ringStore 每会话环形缓冲，采集全部消息供上下文注入与工具桥查询
type ringStore struct {
	mu    sync.Mutex
	cap   int
	rings map[string][]ringEntry
	seq   atomic.Uint64
	read  map[string]uint64 // 每会话已注入提示的游标
}

func newRingStore(capacity int) *ringStore {
	if capacity <= 0 {
		capacity = 60
	}
	return &ringStore{cap: capacity, rings: map[string][]ringEntry{}, read: map[string]uint64{}}
}

func describeMessage(msg *message.Message) (kind, content string) {
	switch msg.GetType().GetCode() {
	case message.TypeText.Code:
		return "text", msg.GetText().GetContent()
	case message.TypeImage.Code:
		return "image", "[图片] " + msg.GetContent()
	case message.TypeEmoji.Code:
		return "emoji", "[表情] " + msg.GetEmoji().GetDesc()
	case message.TypeVoice.Code:
		return "voice", "[语音] " + msg.GetContent()
	case message.TypeVideo.Code, message.TypeTinyVideo.Code:
		return "video", "[视频] " + msg.GetContent()
	case message.TypeAppQuote.Code:
		return "quote", "[引用] " + msg.GetContent()
	case message.TypeAppLink.Code, message.TypeApplication.Code:
		return "app", "[链接] " + msg.GetApp().GetTitle()
	case message.TypeSystem.Code, message.TypeSystemTip.Code:
		return "system", "[系统] " + msg.GetContent()
	default:
		return "other", msg.GetContent()
	}
}

func speakerName(msg *message.Message) string {
	if m := msg.GetMember(); m != nil && m.GetDisplayName() != "" {
		return m.GetDisplayName()
	}
	if m := msg.GetMember(); m != nil && m.GetNickname() != "" {
		return m.GetNickname()
	}
	return msg.GetSender().GetNickname()
}

func (r *ringStore) add(session string, msg *message.Message) {
	kind, content := describeMessage(msg)
	if kind == "system" {
		return
	}
	e := ringEntry{Seq: r.seq.Add(1), Speaker: speakerName(msg), Kind: kind, Content: content}
	r.mu.Lock()
	defer r.mu.Unlock()
	ring := append(r.rings[session], e)
	if len(ring) > r.cap {
		ring = ring[len(ring)-r.cap:]
	}
	r.rings[session] = ring
}

// drainSince 返回上次注入之后的新消息（并推进游标），拼装为上下文块
func (r *ringStore) drainForPrompt(session string, maxChars int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ring := r.rings[session]
	cursor := r.read[session]
	var lines []string
	var newest uint64
	for _, e := range ring {
		if e.Seq <= cursor {
			continue
		}
		newest = e.Seq
		lines = append(lines, e.Speaker+": "+e.Content)
	}
	if len(lines) == 0 {
		return ""
	}
	if newest > 0 {
		r.read[session] = newest
	}
	// 从尾部取不超过 maxChars 的内容
	out := ""
	if maxChars <= 0 {
		maxChars = 1500
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if len(out)+len(lines[i])+1 > maxChars {
			out = "…(更早的略)\n" + out
			break
		}
		if out == "" {
			out = lines[i]
		} else {
			out = lines[i] + "\n" + out
		}
	}
	return out
}

// snapshot 只读查询（工具桥用），返回最近 limit 条
func (r *ringStore) snapshot(session string, limit int) []ringEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	ring := r.rings[session]
	if limit <= 0 || limit > len(ring) {
		limit = len(ring)
	}
	out := make([]ringEntry, limit)
	copy(out, ring[len(ring)-limit:])
	return out
}
