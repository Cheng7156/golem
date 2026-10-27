package main

import (
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sbgayhub/golem/sdk/chatroom"
	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

// fakeAbility 记录 Send 的消息能力桩
type fakeAbility struct {
	sends []*message.Message
}

func (f *fakeAbility) Send(msg *message.Message) (*message.Send_Response, error) {
	f.sends = append(f.sends, msg)
	return &message.Send_Response{NewId: 1}, nil
}
func (f *fakeAbility) Forward(*message.Message, string) (*message.Forward_Response, error) {
	return &message.Forward_Response{}, nil
}
func (f *fakeAbility) Revoke(string, uint64) (*message.Revoke_Response, error) {
	return &message.Revoke_Response{}, nil
}
func (f *fakeAbility) Download(*message.Message) (io.ReadCloser, error) { return nil, nil }

func newFunTestPlugin(t *testing.T) (*hermesPlugin, *fakeAbility) {
	t.Helper()
	s := newTestStore(t)
	fake := &fakeAbility{}
	p := &hermesPlugin{store: s}
	p.Config.CrowEnabled = true // 零值 Config 三开关为 false，测试显式开（OnLoad 实际来自 defaultConfig）
	p.Config.GrudgeEnabled = true
	p.Config.MoodEnabled = true
	p.metrics = newMetrics()
	p.out = newOutflow(fake, &p.Config)
	p.Config.MinReplyIntervalSec = 0
	// 与 OnLoad 相同的机器人时钟回调装配（冷场看护依赖）
	p.out.onSent = func(chat, text string) {
		if p.fun != nil {
			p.fun.noteBot(chat, text, time.Now())
		}
	}
	p.fun = newFunEngine(p)
	// 注册活跃群（chatsSnapshot 的数据源是决策流）
	p.metrics.addDecision(decisionRec{Chat: "g@chatroom", ChatName: "测试群", IsGroup: true})
	t.Cleanup(func() { p.fun.Stop() })
	return p, fake
}

// TestCrowTickFires 引擎接线回归：v0.7.0 曾漏掉 newFunEngine 调用导致三件套静默死亡。
// 打鸣时刻由日期种子确定性决定，逐分钟扫描 6:30-9:29 必然恰好触发一次。
func TestCrowTickFires(t *testing.T) {
	p, fake := newFunTestPlugin(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 6, 30, 0, 0, now.Location())
	for i := 0; i < 180; i++ {
		p.fun.crowTick(base.Add(time.Duration(i) * time.Minute))
	}
	if len(fake.sends) != 1 {
		t.Fatalf("打鸣应恰好一次，得 %d", len(fake.sends))
	}
	cry := fake.sends[0].GetContent()
	known := append(append([]string{}, crowCries...), crowCriesAlt...)
	if !slices.Contains(known, cry) {
		t.Fatalf("叫声不在打鸣文案池: %q", cry)
	}
	if fake.sends[0].GetReceiver().GetUsername() != "g@chatroom" {
		t.Fatalf("打鸣发错了聊天: %q", fake.sends[0].GetReceiver().GetUsername())
	}
	// 当日去重：再扫一轮不再发
	for i := 0; i < 180; i++ {
		p.fun.crowTick(base.Add(time.Duration(i) * time.Minute))
	}
	if len(fake.sends) != 1 {
		t.Fatalf("同日不应重复打鸣，得 %d", len(fake.sends))
	}
}

func TestMaybeGrudgeRecords(t *testing.T) {
	p, fake := newFunTestPlugin(t)
	msg := &message.Message{
		Id: 42, Type: message.TypeText,
		Sender: &contact.Contact{Username: "g@chatroom", Type: contact.ContactType_CONTACT_TYPE_CHATROOM},
		Member: &chatroom.Member{Username: "wxid_a", DisplayName: "张三"},
		Data:   &message.Message_Text{Text: &message.TextData{Content: "闭嘴吧你"}},
	}
	p.fun.maybeGrudge(msg, "g@chatroom")
	if len(fake.sends) != 0 {
		t.Fatal("入账不应发消息")
	}
	p.store.mu.Lock()
	var n int
	_ = p.store.db.QueryRow("SELECT COUNT(*) FROM grudges").Scan(&n)
	var reason string
	_ = p.store.db.QueryRow("SELECT reason FROM grudges LIMIT 1").Scan(&reason)
	p.store.mu.Unlock()
	if n != 1 || !strings.Contains(reason, "闭嘴") {
		t.Fatalf("记仇应入账 1 条，得 %d 条 reason=%q", n, reason)
	}
	// 无关键词不入账
	msg.Data = &message.Message_Text{Text: &message.TextData{Content: "今天天气不错"}}
	p.fun.maybeGrudge(msg, "g@chatroom")
	p.store.mu.Lock()
	_ = p.store.db.QueryRow("SELECT COUNT(*) FROM grudges").Scan(&n)
	p.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("无关键词不应入账，得 %d", n)
	}
}

func TestMoodQuietHours(t *testing.T) {
	p, fake := newFunTestPlugin(t)
	afternoon := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 14, 0, 0, 0, time.Local)
	p.fun.maybeMood(afternoon)
	if len(fake.sends) != 0 {
		t.Fatal("白天不应 emo")
	}
	// 深夜窗口内调用不应 panic（4% 概率发送，不做确定性断言）
	night := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 23, 0, 0, 0, time.Local)
	p.fun.maybeMood(night)
}

func TestMentionsOtherMember(t *testing.T) {
	self := &contact.SelfInfo{Username: "wxid_bot", Nickname: "小赫"}
	mk := func(reminds []string, content string) *message.Message {
		m := &message.Message{Type: message.TypeText,
			Sender: &contact.Contact{Username: "g@chatroom", Type: contact.ContactType_CONTACT_TYPE_CHATROOM},
			Data:   &message.Message_Text{Text: &message.TextData{Content: content}}}
		if reminds != nil {
			m.Data = &message.Message_Text{Text: &message.TextData{Content: content, Reminds: reminds}}
		}
		return m
	}
	cases := []struct {
		name string
		msg  *message.Message
		want bool
	}{
		{"只@别人", mk([]string{"小美"}, "@小美 画一张狗"), true},
		{"只@我", mk([]string{"小赫"}, "@小赫 画一张狗"), false},
		{"@我和别人混合", mk([]string{"小赫", "小美"}, "@小赫 @小美 画一张狗"), false},
		{"无@纯文本", mk(nil, "画一张狗"), false},
		{"无reminds正文兜底@别人", mk(nil, "@小美 画一张狗"), true},
		{"无reminds正文@我", mk(nil, "@小赫 画一张狗"), false},
	}
	for _, c := range cases {
		if got := mentionsOtherMember(c.msg, self); got != c.want {
			t.Fatalf("%s: 期望 %v 得 %v", c.name, c.want, got)
		}
	}
}

func TestSessionBucket(t *testing.T) {
	p := &hermesPlugin{}
	p.Config.SessionRotateMin = 0
	if got := p.sessionBucket(); got != "" {
		t.Fatalf("rotate=0 应无后缀，得 %q", got)
	}
	p.Config.SessionRotateMin = 60
	b1 := p.sessionBucket()
	if b1 == "" || !strings.HasPrefix(b1, ":") {
		t.Fatalf("分桶后缀格式异常: %q", b1)
	}
	b2 := p.sessionBucket()
	if b1 != b2 {
		t.Fatalf("同一分钟内桶号应稳定: %q vs %q", b1, b2)
	}
	// 桶宽正确：桶号差 1 应等于 rotate 分钟
	p.Config.SessionRotateMin = 120
	b3 := p.sessionBucket()
	n1, _ := strconv.ParseInt(strings.TrimPrefix(b1, ":"), 10, 64)
	n3, _ := strconv.ParseInt(strings.TrimPrefix(b3, ":"), 10, 64)
	if n1 == n3 {
		t.Fatal("不同桶宽应产生不同桶号")
	}
}

// TestMutedAnyMemberWake 静音群：任意成员 @/引用 可唤醒，普通消息仍静默
func TestMutedAnyMemberWake(t *testing.T) {
	p, _ := newFunTestPlugin(t)
	_ = p.store.saveChatSettings(chatSettings{Chat: "g@chatroom", Mute: boolPtr(true)})
	mk := func(mentioned, quoted bool, speaker string) *incoming {
		in := &incoming{IsGroup: true, GroupID: "g@chatroom", Receiver: "g@chatroom",
			Speaker: speaker, SpeakerID: "wxid_" + speaker, Text: "hi"}
		in.Mentioned, in.QuotedBot = mentioned, quoted
		return in
	}
	if resp, _, _, why := p.decide(mk(false, false, "路人")); resp {
		t.Fatalf("静音群普通消息应忽略, got %s", why)
	}
	if resp, _, _, _ := p.decide(mk(true, false, "路人")); !resp {
		t.Fatal("任意成员 @ 应唤醒静音群")
	}
	if resp, _, _, _ := p.decide(mk(false, true, "路人")); !resp {
		t.Fatal("任意成员引用应唤醒静音群")
	}
}
