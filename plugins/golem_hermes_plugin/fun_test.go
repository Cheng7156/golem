package main

import (
	"io"
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
	p.Config.CrowEnabled = true  // 零值 Config 三开关为 false，测试显式开（OnLoad 实际来自 defaultConfig）
	p.Config.GrudgeEnabled = true
	p.Config.MoodEnabled = true
	p.metrics = newMetrics()
	p.out = newOutflow(fake, &p.Config)
	p.Config.MinReplyIntervalSec = 0
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
	if !strings.Contains(cry, "喔") && !strings.Contains(cry, "咯") && !strings.Contains(cry, "咕") {
		t.Fatalf("叫声不像打鸣: %q", cry)
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
