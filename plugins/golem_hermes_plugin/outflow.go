package main

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

const (
	maxEmojiBytes  = 500 * 1024
	maxImageBytes  = 3 * 1024 * 1024 // gRPC 默认单消息 4MB，留余量
	minReplyBackup = 1024            // 每会话最小回复间隔兜底
)

// outflow 出站发送收口：并发槽、会话节流、分段、引用、表情、图片
type outflow struct {
	msg   message.Ability
	cfg   *Config
	slots chan struct{}

	mu   sync.Mutex
	last map[string]time.Time
}

func newOutflow(m message.Ability, cfg *Config) *outflow {
	slots := cfg.OutboundSlots
	if slots <= 0 {
		slots = 4
	}
	return &outflow{msg: m, cfg: cfg, slots: make(chan struct{}, slots), last: map[string]time.Time{}}
}

func (o *outflow) throttle(session string) {
	interval := time.Duration(o.cfg.MinReplyIntervalSec) * time.Second
	if interval <= 0 {
		return
	}
	o.mu.Lock()
	wait := interval - time.Since(o.last[session])
	if wait > 0 {
		o.mu.Unlock()
		time.Sleep(wait)
		o.mu.Lock()
	}
	o.last[session] = time.Now()
	o.mu.Unlock()
}

func (o *outflow) sendText(receiver, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	o.slots <- struct{}{}
	defer func() { <-o.slots }()
	o.throttle(receiver)
	_, err := o.msg.Send(&message.Message{
		Type:     message.TypeText,
		Receiver: &contact.Contact{Username: receiver},
		Content:  text,
	})
	return err
}

// sendQuote 引用回复：TypeApplication + SubType 57 + refermsg XML
// （宿主 Send 分支不认 TypeAppQuote(4957)，必须走 App XML，这是全仓唯一先例实现）
func (o *outflow) sendQuote(t *task, text string) error {
	if t.InMsgID == 0 {
		return errors.New("缺少被引用消息ID")
	}
	text = strings.TrimSpace(text)
	o.slots <- struct{}{}
	defer func() { <-o.slots }()
	o.throttle(t.Receiver)
	_, err := o.msg.Send(&message.Message{
		Type:     message.TypeApplication,
		Receiver: &contact.Contact{Username: t.Receiver},
		Content:  text,
		Data: &message.Message_App{App: &message.AppData{
			SubType: 57,
			Title:   text,
			Xml:     buildQuoteXML(t, text),
		}},
	})
	return err
}

func buildQuoteXML(t *task, text string) string {
	from := t.Receiver
	if from == "" {
		from = t.GroupID
	}
	speakerID := t.SpeakerID
	if speakerID == "" {
		speakerID = from
	}
	quoted := t.InContent
	if quoted == "" {
		quoted = t.Text
	}
	// 群消息 content 带有 "昵称:\n" 前缀，refermsg 里去掉
	if i := strings.Index(quoted, ":\n"); i > 0 && i < 40 {
		quoted = quoted[i+2:]
	}
	quotedType := t.InTypeCode
	if quotedType == 0 {
		quotedType = message.TypeText.Code
	}
	return fmt.Sprintf(
		`<appmsg appid="" sdkver="0"><title>%s</title><type>57</type><refermsg><type>%d</type><svrid>%d</svrid><fromusr>%s</fromusr><chatusr>%s</chatusr><displayname>%s</displayname><createtime>%d</createtime><content>%s</content></refermsg></appmsg>`,
		xmlEscape(text), quotedType, t.InMsgID, xmlEscape(from), xmlEscape(speakerID), xmlEscape(t.Speaker), t.InTs, xmlEscape(quoted))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (o *outflow) sendEmoji(receiver string, data []byte) error {
	if len(data) == 0 || len(data) > maxEmojiBytes {
		return fmt.Errorf("表情大小不合规: %d bytes", len(data))
	}
	o.slots <- struct{}{}
	defer func() { <-o.slots }()
	o.throttle(receiver)
	_, err := o.msg.Send(&message.Message{
		Type:     message.TypeEmoji,
		Receiver: &contact.Contact{Username: receiver},
		Data: &message.Message_Emoji{Emoji: &message.EmojiData{
			Media: &message.Media{Data: data, Size: uint32(len(data))},
		}},
	})
	return err
}

func (o *outflow) sendImage(receiver string, data []byte) error {
	if len(data) == 0 || len(data) > maxImageBytes {
		return fmt.Errorf("图片大小不合规: %d bytes", len(data))
	}
	o.slots <- struct{}{}
	defer func() { <-o.slots }()
	o.throttle(receiver)
	_, err := o.msg.Send(&message.Message{
		Type:     message.TypeImage,
		Receiver: &contact.Contact{Username: receiver},
		Data: &message.Message_Image{Image: &message.ImageData{
			Media: &message.Media{Data: data, Size: uint32(len(data))},
		}},
	})
	return err
}

// ---- 回复内容解析：文本 + 内联图片(data URL) + hermes MEDIA: 本地文件 ----

type replyPart struct {
	Kind string // text | image
	Text string
	Data []byte
}

var dataImgRe = regexp.MustCompile(`!\[[^\]]*\]\(data:(image/[a-zA-Z0-9.+\-]+);base64,([A-Za-z0-9+/=\s]+)\)`)

// mediaPathRe hermes 的 MEDIA:<本地路径> 约定（图片生成等工具产物未转 data URL 时漏出）
var mediaPathRe = regexp.MustCompile(`(?mi)^\s*MEDIA:[ \t]*(\S+\.(?:png|jpe?g|gif|webp))[ \t]*$`)

// bareWinPathRe 防泄漏：独立的 Windows 绝对路径行绝不发进微信
var bareWinPathRe = regexp.MustCompile(`(?mi)^\s*[A-Za-z]:\\[^\s]*\.(?:png|jpe?g|gif|webp|tmp|jpg)[ \t]*$`)

func parseMediaParts(text string) []replyPart {
	var imgs [][]byte
	text, imgs = extractMediaFiles(text, mediaPathRe)
	var extra [][]byte
	text, extra = extractMediaFiles(text, bareWinPathRe)
	imgs = append(imgs, extra...)

	matches := dataImgRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 && len(imgs) == 0 {
		return []replyPart{{Kind: "text", Text: text}}
	}
	var parts []replyPart
	for _, img := range imgs {
		parts = append(parts, replyPart{Kind: "image", Data: img})
	}
	last := 0
	for _, m := range matches {
		if m[0] > last {
			if seg := strings.TrimSpace(text[last:m[0]]); seg != "" {
				parts = append(parts, replyPart{Kind: "text", Text: seg})
			}
		}
		clean := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, text[m[4]:m[5]])
		if data, err := base64.StdEncoding.DecodeString(clean); err == nil && len(data) > 0 {
			parts = append(parts, replyPart{Kind: "image", Data: data})
		}
		last = m[1]
	}
	if rest := strings.TrimSpace(text[last:]); rest != "" {
		parts = append(parts, replyPart{Kind: "text", Text: rest})
	}
	return parts
}

// extractMediaFiles 把 MEDIA:<路径> / 裸路径行替换为本机读出的图片字节；
// 文件不可读、超限或非图片时整行静默丢弃——服务器路径绝不发进微信
func extractMediaFiles(text string, re *regexp.Regexp) (string, [][]byte) {
	matches := re.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	var imgs [][]byte
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m[0]])
		path := strings.TrimSpace(text[m[2]:m[3]])
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 && len(data) <= maxImageBytes && sniffImage(data) != "" {
			imgs = append(imgs, data)
			b.WriteString(" ")
		}
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String(), imgs
}

// segmentReply 长文本分段（按 rune 计数，优先自然断点）
func segmentReply(text string, maxChars, maxSegs int) []string {
	ra := []rune(text)
	if maxChars <= 0 {
		maxChars = 1200
	}
	if maxSegs <= 0 {
		maxSegs = 3
	}
	if len(ra) <= maxChars {
		return []string{text}
	}
	var segs []string
	remain := text
	for len([]rune(remain)) > maxChars && len(segs) < maxSegs-1 {
		chunk := string([]rune(remain)[:maxChars])
		cut := -1
		for _, sep := range []string{"\n\n", "\n", "。", "！", "？", "；", "，", " "} {
			if i := strings.LastIndex(chunk, sep); i > maxChars/2 {
				cut = i + len(sep)
				break
			}
		}
		if cut < 0 {
			cut = maxChars
		}
		segs = append(segs, strings.TrimSpace(string([]rune(remain)[:cut])))
		remain = string([]rune(remain)[cut:])
	}
	if strings.TrimSpace(remain) != "" {
		segs = append(segs, strings.TrimSpace(remain))
	}
	return segs
}
