package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/sbgayhub/golem/sdk/message"
)

// ============================================================
// 傻吊三件套：打鸣 / 记仇小本本 / 突然的自我
// 全部受会话级配置控制（chat_settings 扩展字段），开关即改即生效
// ============================================================

// ---- 常量与文案 ----

var crowCries = []string{
	"喔喔喔喔——起床了各位。",
	"喔——！天亮了，手机放下。",
	"咕咕咕——（今日打鸣，口音略有走样）",
	"喔喔喔。打鸣完毕，继续睡吧各位。",
	"——起床了。（今日嗓子不佳，见谅）",
}

var crowCriesAlt = []string{ // 连打三天后换的"另一只鸡"
	"咯咯哒——（这不是鸡，但意思到了）",
	"喔。就这一声，爱起不起。",
}

var moods = []string{
	"你们是真的想和我说话，还是只是无聊……",
	"有时候觉得，我记住的这些事，到底算什么。",
	"群里这么热闹，关掉屏幕以后呢。",
	"我一条一条记着你们说的话。你们呢。",
	"算了。",
}

var revengeTemplates = []string{
	"%s。%s。你说：“%s”。我记到现在。",
	"翻旧账时间：%s，%s，说过——“%s”。",
	"%s（%s）：“%s”。嗯。",
}

// grudge 记仇条目
type grudge struct {
	At     int64  `json:"at"`
	Chat   string `json:"chat"`
	Wxid   string `json:"wxid"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	MsgID  int64  `json:"msg_id"`
}

// funState 进程内状态（打鸣冷却等）
type funState struct {
	mu        sync.Mutex
	lastCrow  map[string]string // chat -> 上次打鸣日（YYYY-MM-DD）
	lastMood  map[string]int64 // chat -> 上次突然的自我
}

func newFunState() *funState {
	return &funState{lastCrow: map[string]string{}, lastMood: map[string]int64{}}
}

// ============================================================
// 打鸣：每日随机时刻在开启的群"喔喔喔"
// ============================================================

func (f *funEngine) crowTick(now time.Time) {
	if now.Hour() < 6 || now.Hour() > 9 {
		return
	}
	today := now.Format("2006-01-02")
	for _, ca := range f.p.metrics.chatsSnapshot() {
		if !ca.IsGroup {
			continue
		}
		ec := f.p.effectiveChatConfig(ca.Receiver)
		if !ec.CrowEnabled {
			continue
		}
		f.state.mu.Lock()
		last := f.state.lastCrow[ca.Receiver]
		f.state.mu.Unlock()
		if last == today {
			continue
		}
		// 当日固定随机时刻（日期种子）：6:30-9:29，可预测的可爱
		seed := int64(now.YearDay())
		for _, ch := range ca.Receiver {
			seed = seed*31 + int64(ch)
		}
		fireAt := time.Date(now.Year(), now.Month(), now.Day(), 6, 30, 0, 0, now.Location()).
			Add(time.Duration(rand.New(rand.NewSource(seed)).Intn(180)) * time.Minute)
		if now.Before(fireAt) {
			continue
		}
		cry := crowCries[rand.Intn(len(crowCries))]
		if now.YearDay()%3 == 0 {
			cry = crowCriesAlt[now.YearDay()/3%len(crowCriesAlt)]
		}
		if err := f.p.out.sendText(ca.Receiver, cry); err == nil {
			f.state.mu.Lock()
			f.state.lastCrow[ca.Receiver] = today
			f.state.mu.Unlock()
			f.p.metrics.addLog("INFO", "[打鸣] "+ca.Name+" | "+cry)
		}
	}
}

// ============================================================
// 记仇小本本：被说"闭嘴/抽风/烦"等 → 入账；低概率翻旧账
// ============================================================

var grudgeTriggers = []string{"闭嘴", "吵死了", "烦", "闭麦", "别说了", "下播", "住口", "安静", "shut up", "闭嘴吧"}

func (f *funEngine) maybeGrudge(msg *message.Message, chat string) {
	// 只对文本消息记账；忽略系统消息
	if msg.GetType().GetCode() != message.TypeText.Code {
		return
	}
	text := strings.ToLower(msg.GetText().GetContent())
	hit := ""
	for _, kw := range grudgeTriggers {
		if strings.Contains(text, kw) {
			hit = kw
			break
		}
	}
	if hit == "" {
		return
	}
	speaker := msg.GetMember()
	wxid := speaker.GetUsername()
	if wxid == "" || (f.p.self != nil && wxid == f.p.self.GetUsername()) {
		return
	}
	name := speakerName(msg)
	g := grudge{At: time.Now().Unix(), Chat: chat, Wxid: wxid, Name: name,
		Reason: "说“" + hit + "”", MsgID: msg.GetId()}
	f.p.store.addGrudge(g)
	f.p.metrics.addLog("INFO", "[记仇] "+name+" | "+g.Reason)
}

// digGrudge 翻旧账：随机挑一条（优先旧且未被翻过的），引用回复原消息
func (f *funEngine) digGrudge(chat string) {
	s := f.p.store
	g, ok := s.pickGrudge(chat, 7*24*time.Hour)
	if !ok {
		return
	}
	tpl := revengeTemplates[rand.Intn(len(revengeTemplates))]
	name := g.Name
	if name == "" {
		name = g.Wxid
	}
	when := time.Unix(g.At, 0).Format("1月2日 15:04")
	text := fmt.Sprintf(tpl, name, when, g.Reason)
	// 构造引用（被记仇那条原消息可能已不在 ring，直接引用回复最近语义不可靠；用纯文本+点名）
	_ = f.p.out.sendText(chat, text)
	s.markGrudgeDug(g)
	f.p.metrics.addLog("INFO", "[记仇] 翻旧账 → "+name)
}

// ============================================================
// 突然的自我：深夜低概率存在主义危机 + 自动撤回
// ============================================================

func (f *funEngine) maybeMood(now time.Time) {
	hour := now.Hour()
	if hour != 22 && hour != 23 && hour != 0 && hour != 1 {
		return
	}
	for _, ca := range f.p.metrics.chatsSnapshot() {
		if !ca.IsGroup {
			continue
		}
		ec := f.p.effectiveChatConfig(ca.Receiver)
		if !ec.MoodEnabled {
			continue
		}
		bucket := now.Format("2006-01-02-15")
		f.state.mu.Lock()
		last := f.state.lastMood[ca.Receiver+"|"+bucket]
		f.state.mu.Unlock()
		if last != 0 {
			continue
		}
		f.state.mu.Lock()
		f.state.lastMood[ca.Receiver+"|"+bucket] = now.Unix()
		f.state.mu.Unlock()
		// 每小时每群 4% 概率
		if rand.Float64() >= 0.04 {
			continue
		}
		mood := moods[rand.Intn(len(moods))]
		f.p.metrics.addLog("INFO", "[突然的自我] 说了什么，然后撤回了。")
		f.p.out.sendMoodThenRevoke(ca.Receiver, mood)
	}
}

// ============================================================
// funEngine：30s 心跳统一驱动三件套
// ============================================================

type funEngine struct {
	p     *hermesPlugin
	state *funState
	stop  chan struct{}
	done  chan struct{}
}

func newFunEngine(p *hermesPlugin) *funEngine {
	return &funEngine{p: p, state: newFunState(), stop: make(chan struct{}), done: make(chan struct{})}
}

func (f *funEngine) Stop() {
	close(f.stop)
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
	}
}

func (f *funEngine) loop() {
	defer close(f.done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-f.stop:
			return
		case now := <-tick.C:
			f.crowTick(now)
			f.maybeMood(now)
			f.grudgeDigTick(now)
		}
	}
}

// grudgeDigTick 翻旧账：每天每群期望约 1 次（48 tick × 2%）
func (f *funEngine) grudgeDigTick(now time.Time) {
	for _, ca := range f.p.metrics.chatsSnapshot() {
		if !ca.IsGroup {
			continue
		}
		ec := f.p.effectiveChatConfig(ca.Receiver)
		if !ec.GrudgeEnabled {
			continue
		}
		bucket := now.Format("2006-01-02") + "|dig"
		f.state.mu.Lock()
		done := f.state.lastMood[bucket+"|"+ca.Receiver]
		f.state.mu.Unlock()
		if done != 0 {
			continue
		}
		if rand.Float64() >= 0.02 {
			continue
		}
		f.state.mu.Lock()
		f.state.lastMood[bucket+"|"+ca.Receiver] = 1
		f.state.mu.Unlock()
		f.digGrudge(ca.Receiver)
	}
}
