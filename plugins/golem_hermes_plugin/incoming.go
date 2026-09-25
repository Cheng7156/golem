package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
)

// incoming 归一化后的入站消息
type incoming struct {
	RingKey    string // 观察上下文键（群共享，供 ring buffer）
	HermesKey  string // Hermes 会话键（受 group_sessions_per_user 影响）
	Receiver   string
	IsGroup    bool
	GroupID    string
	SpeakerID  string
	Speaker    string
	IsOwner    bool
	Text       string
	Quote      quoteInfo
	Mentioned  bool
	QuotedBot  bool
	InMsgID    int64
	InTs       uint32
	InTypeCode int32
	InContent  string
}

// decide 触发决策（唯一判定入口，返回结果同时用于埋点）
func (p *hermesPlugin) decide(in *incoming) (respond, ambient bool, trigger, ignore string) {
	// 成员级：屏蔽名单（主人亦不免，除非先解除屏蔽）
	if p.store.isMemberIgnored(in.SpeakerID) {
		return false, false, "", "成员被屏蔽"
	}
	ec := p.effectiveChatConfig(in.GroupID)
	if !in.IsGroup {
		if p.Config.OwnerOnlyPrivate && !in.IsOwner {
			return false, false, "", "陌生人私聊(owner_only_private)"
		}
		return true, false, "private", ""
	}
	// 群级静音：仅主人 @/引用 可穿透唤醒
	if ec.Muted && !(in.IsOwner && (in.Mentioned || in.QuotedBot)) {
		return false, false, "", "群已静音"
	}
	if in.Mentioned {
		return true, false, "mention", ""
	}
	if in.QuotedBot {
		return true, false, "quote", ""
	}
	if in.IsOwner && p.Config.OwnerBypassAmbient {
		return true, true, "owner-bypass", ""
	}
	if ec.FreeResponse {
		return true, false, "whitelist", ""
	}
	if p.Config.DailyTokenCap > 0 && p.store.tokenExceeded(p.Config.DailyTokenCap) {
		return false, false, "", "预算熔断"
	}
	if hit, why := p.ambientRoll(in.GroupID, ec.AmbientRate, ec.AmbientCooldownSec); !hit {
		return false, false, "", why
	}
	if len([]rune(in.Text)) < p.Config.MinAmbientLen {
		return false, false, "", "消息过短"
	}
	return true, true, "ambient", ""
}

// OnEvent 事件入口：毫秒级完成 采集→去重→决策→入队，绝不做长阻塞工作
func (p *hermesPlugin) OnEvent(e *plugin.Event) (bool, error) {
	msg := e.GetMessage()
	if msg == nil {
		return false, nil
	}
	session := e.GetSender()
	if session == "" {
		return false, nil
	}
	// 忽略自己的消息（echo）
	if msg.GetSender().GetType() == contact.ContactType_CONTACT_TYPE_SELF ||
		(p.self != nil && msg.GetSender().GetUsername() == p.self.GetUsername()) {
		return false, nil
	}

	// 1. 全量采集群/私聊上下文（AlwaysRun 保证劫持期间也能记录）
	p.rings.add(session, msg)

	// 1.4 记仇小本本：被嫌弃则入账（同步轻量判断，写库在 store 锁内很快）
	if p.fun != nil && (p.Config.GrudgeEnabled || p.effectiveChatConfig(session).GrudgeEnabled) {
		p.fun.maybeGrudge(msg, session)
	}

	// 1.5 消息归档（画像与群史官的持久语料；异步非阻塞）
	if p.Config.ProfileEnabled {
		kind, content := describeMessage(msg)
		p.store.archive(archiveMsg{
			Ts: time.Now().Unix(), Chat: session,
			Wxid: msg.GetMember().GetUsername(), Name: speakerName(msg),
			Kind: kind, Content: previewOf(content, 200),
		})
	}

	// 2. 表情收集（异步，不阻塞事件链）；新收集立即唤醒打标
	if p.Config.StickerEnabled && msg.GetType().GetCode() == message.TypeEmoji.Code {
		go func() {
			if p.store.collectSticker(msg.GetEmoji()) && p.tagger != nil {
				p.tagger.Wake()
			}
		}()
	}

	// 3. 按消息 ID 去重
	if msg.GetId() != 0 && !p.markSeen(msg.GetId()) {
		p.recordDecision(session, msg, "忽略:重复消息", "", "")
		return false, nil
	}

	// 4. 构建归一化消息
	in := p.buildIncoming(session, msg)
	if in == nil {
		p.recordDecision(session, msg, "忽略:非触发类型("+msg.GetType().Desc+")", "", "")
		return false, nil
	}

	// 5. 触发决策
	respond, ambient, trigger, ignore := p.decide(in)
	if !respond {
		p.recordDecision(session, msg, "忽略:"+ignore, "", "")
		return false, nil
	}
	p.recordDecision(session, msg, "响应("+trigger+")", trigger, p.routeProfile(in.Text))

	// 6. 图片消息且被触发时下载字节（供 vision），限 30s
	var image []byte
	if msg.GetType().GetCode() == message.TypeImage.Code {
		image = p.downloadImage(msg, 30*time.Second)
	}

	profile := p.routeProfile(in.Text)
	if image != nil && p.Config.VisionProfile != "" {
		profile = p.Config.VisionProfile // 视觉模型才吃得了图片，带图请求走视觉通道
	}

	t := &task{
		SessionKey: in.HermesKey,
		RingKey:    in.RingKey,
		Receiver:   in.Receiver,
		IsGroup:    in.IsGroup,
		GroupID:    in.GroupID,
		SpeakerID:  in.SpeakerID,
		Speaker:    in.Speaker,
		IsOwner:    in.IsOwner,
		Text:       in.Text,
		Quote:      in.Quote.Content,
		Mentioned:  in.Mentioned,
		QuotedBot:  in.QuotedBot,
		Ambient:    ambient,
		Trigger:    trigger,
		Profile:    profile,
		Image:      image,
		ImageMime:  "image/jpeg",
		InMsgID:    in.InMsgID,
		InTs:       in.InTs,
		InTypeCode: in.InTypeCode,
		InContent:  in.InContent,
	}
	p.disp.enqueue(t)
	return true, nil
}

func (p *hermesPlugin) buildIncoming(session string, msg *message.Message) *incoming {
	sender := msg.GetSender()
	if sender == nil || sender.GetUsername() == "" {
		return nil
	}
	isGroup := sender.GetType() == contact.ContactType_CONTACT_TYPE_CHATROOM
	in := &incoming{
		RingKey:    session,
		Receiver:   sender.GetUsername(),
		IsGroup:    isGroup,
		InMsgID:    msg.GetId(),
		InTs:       msg.GetTimestamp(),
		InTypeCode: msg.GetType().GetCode(),
		InContent:  msg.GetContent(),
		Quote:      extractQuote(msg),
	}
	if isGroup {
		in.GroupID = sender.GetUsername()
		in.SpeakerID = msg.GetMember().GetUsername()
		in.Speaker = sanitizeName(speakerName(msg))
		// 隔离策略：群设置覆盖 > 全局；按发言人拆分或整群共享
		perUser := p.Config.GroupSessionsPerUser
		if ec := p.effectiveChatConfig(in.GroupID); true {
			perUser = ec.PerUserSession
		}
		if perUser && in.SpeakerID != "" {
			in.HermesKey = "wx:group:" + in.GroupID + ":" + in.SpeakerID
		} else {
			in.HermesKey = "wx:group:" + in.GroupID
		}
	} else {
		in.SpeakerID = sender.GetUsername()
		in.Speaker = sanitizeName(displayContact(sender))
		in.HermesKey = "wx:private:" + sender.GetUsername()
	}
	// 主人识别：协议层 wxid 精确比对，与消息文本无关
	in.IsOwner = p.isOwnerWxid(in.SpeakerID)

	switch msg.GetType().GetCode() {
	case message.TypeText.Code:
		in.Text = strings.TrimSpace(msg.GetText().GetContent())
	case message.TypeAppQuote.Code:
		in.Text = strings.TrimSpace(msg.GetContent())
	case message.TypeImage.Code:
		in.Text = "[图片]"
	default:
		return nil
	}
	if in.Text == "" && in.Quote.Content == "" {
		return nil
	}
	self := p.self
	in.Mentioned = isMentionedBot(msg, self)
	in.QuotedBot = isQuotedBot(in.Quote, self)
	// 免@白名单群视为"被@级"交互
	if !in.Mentioned && in.IsGroup && p.inFreeList(in.GroupID) {
		in.Mentioned = true
	}
	return in
}

func (p *hermesPlugin) markSeen(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if len(p.seen) > 4096 {
		for k, ts := range p.seen {
			if now.Sub(ts) > time.Hour {
				delete(p.seen, k)
			}
		}
	}
	if _, ok := p.seen[id]; ok {
		return false
	}
	p.seen[id] = now
	return true
}

func (p *hermesPlugin) inFreeList(groupID string) bool {
	for _, g := range p.Config.FreeResponseChats {
		if g == groupID {
			return true
		}
	}
	return false
}

// ambientRoll 环境参与掷骰：概率 + 群级冷却（可被群设置覆盖）；返回未命中原因供决策埋点
func (p *hermesPlugin) ambientRoll(groupID string, rateOver float64, cooldownOver int) (bool, string) {
	rate := p.Config.AmbientReplyRate
	if rateOver >= 0 {
		rate = rateOver
	}
	cooldown := time.Duration(p.Config.AmbientCooldownSec) * time.Second
	if cooldownOver >= 0 {
		cooldown = time.Duration(cooldownOver) * time.Second
	}
	if rate <= 0 {
		return false, "环境概率为0"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	last := p.ambient[groupID]
	if cooldown > 0 && time.Since(last) < cooldown {
		return false, "群冷却中"
	}
	if rand.Float64() >= rate {
		return false, fmt.Sprintf("概率未中(%.2f)", rate)
	}
	p.ambient[groupID] = time.Now()
	return true, ""
}

// recordDecision 决策埋点
func (p *hermesPlugin) recordDecision(session string, msg *message.Message, outcome, trigger, profile string) {
	if p.metrics == nil {
		return
	}
	sender := msg.GetSender()
	p.metrics.addDecision(decisionRec{
		Chat:      session,
		ChatName:  displayContact(sender),
		IsGroup:   sender.GetType() == contact.ContactType_CONTACT_TYPE_CHATROOM,
		Speaker:   speakerName(msg),
		SpeakerID: msg.GetMember().GetUsername(),
		IsOwner:   p.isOwnerWxid(msg.GetMember().GetUsername()),
		Outcome:   outcome,
		Trigger:   trigger,
		Profile:   profile,
		Preview:   previewOf(msg.GetContent(), 60),
	})
}

// routeProfile 关键词命中走任务通道，否则快速通道
func (p *hermesPlugin) routeProfile(text string) string {
	for _, kw := range p.Config.FullKeywords {
		if kw != "" && strings.Contains(text, kw) {
			return p.Config.FullProfile
		}
	}
	return p.Config.FastProfile
}

func (p *hermesPlugin) downloadImage(msg *message.Message, timeout time.Duration) []byte {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		rc, err := p.message.Download(msg)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, 8*1024*1024))
		ch <- result{data: data, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			slog.Warn("[ghp] 下载图片失败", "err", r.err)
			return nil
		}
		return r.data
	case <-time.After(timeout):
		slog.Warn("[ghp] 下载图片超时")
		return nil
	}
}

// ==================== @ 与引用识别（参照 ai 插件实践） ====================

// sanitizeName 昵称/显示名消毒：剥离控制字符与换行，防伪造身份信封行
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	ra := []rune(s)
	if len(ra) > 32 {
		ra = ra[:32]
	}
	return strings.TrimSpace(string(ra))
}

func displayContact(c *contact.Contact) string {
	if c.GetRemark() != "" {
		return c.GetRemark()
	}
	if c.GetNickname() != "" {
		return c.GetNickname()
	}
	return c.GetUsername()
}

func selfIdentities(self *contact.SelfInfo) []string {
	if self == nil {
		return nil
	}
	out := []string{self.GetUsername(), self.GetNickname(), self.GetAlias()}
	uniq := out[:0]
	for _, v := range out {
		if v != "" {
			uniq = append(uniq, v)
		}
	}
	return uniq
}

func isMentionedBot(msg *message.Message, self *contact.SelfInfo) bool {
	identities := selfIdentities(self)
	if len(identities) == 0 {
		return false
	}
	if text := msg.GetText(); text != nil {
		for _, remind := range text.GetReminds() {
			remind = strings.Trim(strings.TrimSpace(remind), "@")
			for _, identity := range identities {
				if remind != "" && strings.EqualFold(remind, identity) {
					return true
				}
			}
		}
	}
	content := msg.GetContent()
	for _, identity := range identities {
		if strings.Contains(content, "@"+identity) {
			return true
		}
	}
	return false
}

func isQuotedBot(q quoteInfo, self *contact.SelfInfo) bool {
	identities := selfIdentities(self)
	if len(identities) == 0 {
		return false
	}
	for _, value := range []string{q.FromUser, q.ChatUser} {
		for _, identity := range identities {
			if value != "" && strings.EqualFold(strings.TrimSpace(value), identity) {
				return true
			}
		}
	}
	return false
}

type quoteInfo struct {
	FromUser    string
	ChatUser    string
	DisplayName string
	Content     string
	Svrid       string
}

func (q quoteInfo) hasValue() bool {
	return q.FromUser != "" || q.ChatUser != "" || q.DisplayName != "" || q.Content != ""
}

func extractQuote(msg *message.Message) quoteInfo {
	if msg == nil {
		return quoteInfo{}
	}
	if app := msg.GetApp(); app != nil {
		if q := parseQuoteXML(app.GetXml()); q.hasValue() {
			return q
		}
	}
	if raw := msg.GetRaw(); raw != "" {
		var data struct {
			Content struct {
				Value string `json:"value"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(raw), &data); err == nil && data.Content.Value != "" {
			if q := parseQuoteXML(data.Content.Value); q.hasValue() {
				return q
			}
		}
	}
	return quoteInfo{}
}

func parseQuoteXML(raw string) quoteInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return quoteInfo{}
	}
	var data struct {
		AppMsg struct {
			Refer quoteRefer `xml:"refermsg"`
		} `xml:"appmsg"`
		Refer quoteRefer `xml:"refermsg"`
	}
	if err := xml.Unmarshal([]byte(raw), &data); err != nil {
		return quoteInfo{}
	}
	refer := data.AppMsg.Refer
	if !refer.hasValue() {
		refer = data.Refer
	}
	return quoteInfo{
		FromUser:    strings.TrimSpace(refer.FromUser),
		ChatUser:    strings.TrimSpace(refer.ChatUser),
		DisplayName: strings.TrimSpace(refer.DisplayName),
		Content:     strings.TrimSpace(refer.Content),
		Svrid:       strings.TrimSpace(refer.Svrid),
	}
}

type quoteRefer struct {
	DisplayName string `xml:"displayname"`
	FromUser    string `xml:"fromusr"`
	ChatUser    string `xml:"chatusr"`
	Content     string `xml:"content"`
	Svrid       string `xml:"svrid"`
}

func (q quoteRefer) hasValue() bool {
	return q.DisplayName != "" || q.FromUser != "" || q.ChatUser != "" || q.Content != ""
}
