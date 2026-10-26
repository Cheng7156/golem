package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateEngine 可控引擎：Run 阻塞在 gate 上，用于编排调度时序
type gateEngine struct {
	mu     sync.Mutex
	gate   chan struct{} // 关闭则放行所有在途 Run
	once   sync.Once
	blocks int          // 尚未放行的 Run 次数（-1=全部阻塞直到 gate 关闭）
	ran    []runRequest // 收到的请求（prompt 快照）
	calls  int
	script []runEvent // 非 nil 时 Run 同步回放事件流（流式播报测试用）
	text   string
}

// release 幂等放行
func (e *gateEngine) release() { e.once.Do(func() { close(e.gate) }) }

func (e *gateEngine) Run(ctx context.Context, req runRequest, onEvent func(runEvent)) (engineResult, error) {
	e.mu.Lock()
	e.calls++
	e.ran = append(e.ran, req)
	script := e.script
	text := e.text
	b := e.blocks
	if b != 0 {
		if b > 0 {
			e.blocks--
		}
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return engineResult{}, ctx.Err()
		case <-e.gate:
		}
	} else {
		e.mu.Unlock()
	}
	for _, ev := range script {
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if text == "" && len(script) == 0 {
		text = "好的" // 老测试的默认回复
	}
	return engineResult{Text: text}, nil
}
func (e *gateEngine) Complete(ctx context.Context, req runRequest) (engineResult, error) {
	return engineResult{}, nil
}
func (e *gateEngine) Health(ctx context.Context) error { return nil }

func newDispatchTest(t *testing.T) (*dispatcher, *gateEngine, *fakeAbility) {
	t.Helper()
	p, fake := newFunTestPlugin(t)
	eng := &gateEngine{gate: make(chan struct{}), blocks: -1}
	cfg := &p.Config
	cfg.RunTimeoutSec = 10
	cfg.BurstWindowSec = 1
	cfg.BurstStaleMin = 5
	cfg.MaxReplyChars = 200
	cfg.MaxSegments = 2
	p.rings = newRingStore(60)
	d := newDispatcher(cfg, eng, p.out, p.rings, p.store, p.metrics)
	t.Cleanup(eng.release)
	return d, eng, fake
}

func mkTask(chat, trigger, text string) *task {
	return &task{
		SessionKey: "wx:group:" + chat, RingKey: chat, Receiver: chat,
		IsGroup: true, GroupID: chat, SpeakerID: "wxid_s", Speaker: "某人",
		Text: text, Trigger: trigger, Profile: "fast",
	}
}

func waitCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func TestTriggerPriorityGrades(t *testing.T) {
	cases := map[string]int{
		"ambient": 0, "whitelist": 1,
		"mention": 2, "quote": 2, "private": 2, "owner-bypass": 2,
	}
	for trig, want := range cases {
		if got := triggerPriority(trig); got != want {
			t.Fatalf("%s 优先级应 %d，得 %d", trig, want, got)
		}
	}
}

// TestCasualYieldsToInFlight 白名单闲聊不得打断在途 run（@ 才能打断）
func TestCasualYieldsToInFlight(t *testing.T) {
	d, eng, _ := newDispatchTest(t)
	d.enqueue(mkTask("g@x", "mention", "在途的@消息"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return eng.calls == 1 })
	// 白名单闲聊 1：在途是 mention(2)，让路，不打断
	d.enqueue(mkTask("g@x", "whitelist", "闲聊一"))
	// 白名单闲聊 2：在途仍是 mention，继续让路
	d.enqueue(mkTask("g@x", "whitelist", "闲聊二"))
	time.Sleep(400 * time.Millisecond)
	eng.mu.Lock()
	calls := eng.calls
	eng.mu.Unlock()
	if calls != 1 {
		t.Fatalf("闲聊让路后不应有新调用，得 %d", calls)
	}
	// 收尾：放行在途 run
	eng.release()
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.ran) == 1 && eng.calls == 1 })
}

// TestMentionInterruptsCasual 正式消息打断在途闲聊
func TestMentionInterruptsCasual(t *testing.T) {
	d, eng, _ := newDispatchTest(t)
	d.enqueue(mkTask("g@x", "whitelist", "闲聊在途"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return eng.calls == 1 })
	d.enqueue(mkTask("g@x", "mention", "用户@消息"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return eng.calls == 2 })
	// 第一个 run 应以 ctx cancel 收场，第二个正常完成
	eng.release()
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.ran) == 2 })
	if eng.ran[1].Prompt == "" || !strings.Contains(eng.ran[1].Prompt, "用户@消息") {
		t.Fatal("打断后的新任务未运行")
	}
}

// TestBurstMerge 闲聊收揽窗口合并批量
func TestBurstMerge(t *testing.T) {
	d, eng, fake := newDispatchTest(t)
	// 引擎直接放行（不阻塞）
	eng.mu.Lock()
	eng.blocks = 0
	eng.mu.Unlock()
	d.enqueue(mkTask("g@x", "whitelist", "第一批头一条"))
	d.enqueue(mkTask("g@x", "whitelist", "积压消息A"))
	d.enqueue(mkTask("g@x", "whitelist", "积压消息B"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.ran) >= 1 })
	// 头一条进入收揽窗口，A/B 被合并（BatchN 提示注入 prompt）
	eng.mu.Lock()
	prompt := eng.ran[0].Prompt
	calls := eng.calls
	eng.mu.Unlock()
	if !strings.Contains(prompt, "积累的") {
		t.Fatalf("批量提示未注入:\n%s", prompt)
	}
	// 收揽窗口 1s 后批跑完；A/B 不应产生独立 run（总数 1）
	time.Sleep(1600 * time.Millisecond)
	eng.mu.Lock()
	calls = eng.calls
	eng.mu.Unlock()
	if calls != 1 {
		t.Fatalf("批量应一次调用，得 %d 次", calls)
	}
	if len(fake.sends) == 0 {
		t.Fatal("批量回复应已发出")
	}
}

// TestStaleCasualDropped 过期闲聊直接丢弃
func TestStaleCasualDropped(t *testing.T) {
	d, eng, _ := newDispatchTest(t)
	eng.mu.Lock()
	eng.blocks = 0
	eng.mu.Unlock()
	old := mkTask("g@x", "whitelist", "凉透的话题")
	old.CreatedAt = time.Now().Add(-30 * time.Minute)
	d.enqueue(old)
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return eng.calls >= 0 })
	time.Sleep(300 * time.Millisecond)
	eng.mu.Lock()
	calls := eng.calls
	eng.mu.Unlock()
	if calls != 0 {
		t.Fatalf("过期闲聊不应触发 run，得 %d 次", calls)
	}
}

// TestNarrationStreamsAtToolBoundary 叙述过渡句在工具边界实时发群，最终内容（新闻）整发
func TestNarrationStreamsAtToolBoundary(t *testing.T) {
	d, eng, fake := newDispatchTest(t)
	const narr1 = "收到，先去搜一下今天的新闻。"
	const narr2 = "稍等，正在翻。"
	const news = "今日新闻要点如下：一、天气晴。二、市场平稳。三、球队赢了。"
	eng.mu.Lock()
	eng.blocks = 0
	eng.script = []runEvent{
		{Type: evDelta, Text: narr1},
		{Type: evDelta, Text: narr2},
		{Type: evTool, Tool: "web_search"},
		{Type: evDelta, Text: news},
	}
	eng.text = narr1 + narr2 + news
	eng.mu.Unlock()
	d.enqueue(mkTask("g@x", "mention", "今天的新闻"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.ran) == 1 })
	time.Sleep(250 * time.Millisecond)
	if len(fake.sends) != 2 {
		t.Fatalf("应为 叙述+新闻 两条, got %d: %v", len(fake.sends), fake.sends)
	}
	first := fake.sends[0].GetContent()
	if first != narr1+narr2 {
		t.Fatalf("第一条应是工具边界冲刷的叙述:\nwant %q\ngot  %q", narr1+narr2, first)
	}
	second := fake.sends[1].GetContent()
	if second != news {
		t.Fatalf("第二条应是新闻整发:\nwant %q\ngot  %q", news, second)
	}
}

// TestUnclosedImageHeld 叙述里未闭合的内联图标记压住不实时播，结尾一并交付
func TestUnclosedImageHeld(t *testing.T) {
	d, eng, fake := newDispatchTest(t)
	eng.mu.Lock()
	eng.blocks = 0
	eng.script = []runEvent{
		{Type: evDelta, Text: "先看图 ![预览](data:image/png;base64,AAA"},
		{Type: evTool, Tool: "web_search"},
		{Type: evDelta, Text: "好了。"},
	}
	eng.text = "先看图 ![预览](data:image/png;base64,AAA好了。"
	eng.mu.Unlock()
	d.enqueue(mkTask("g@x", "mention", "看图"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.ran) == 1 })
	time.Sleep(250 * time.Millisecond)
	if len(fake.sends) != 1 {
		t.Fatalf("未闭合标记应压住不播（结尾整发一条）, got %d: %v", len(fake.sends), fake.sends)
	}
	if got := fake.sends[0].GetContent(); !strings.Contains(got, "好了。") {
		t.Fatalf("结尾应整发全部文本: %q", got)
	}
}

// TestYieldKeepsLastTask 让路的闲聊不得覆盖会话绑定的 LastTask（画坊甲方张冠李戴修复）
func TestYieldKeepsLastTask(t *testing.T) {
	d, eng, _ := newDispatchTest(t)
	d.enqueue(mkTask("g@x", "mention", "坦然的画图单"))
	waitCond(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return eng.calls == 1 })
	// 另一个 bot 的闲聊让路
	d.enqueue(mkTask("g@x", "whitelist", "小聪明儿插话"))
	time.Sleep(300 * time.Millisecond)
	d.mu.Lock()
	as := d.active["wx:group:g@x"]
	d.mu.Unlock()
	if as == nil || as.LastTask == nil {
		t.Fatal("会话绑定丢失")
	}
	if as.LastTask.Speaker != "某人" || as.LastTask.Text != "坦然的画图单" {
		t.Fatalf("让路覆盖了 LastTask: speaker=%s text=%s", as.LastTask.Speaker, as.LastTask.Text)
	}
	eng.release()
}
