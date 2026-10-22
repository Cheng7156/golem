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
}

// release 幂等放行
func (e *gateEngine) release() { e.once.Do(func() { close(e.gate) }) }

func (e *gateEngine) Run(ctx context.Context, req runRequest, onEvent func(runEvent)) (engineResult, error) {
	e.mu.Lock()
	e.calls++
	e.ran = append(e.ran, req)
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
	return engineResult{Text: "好的"}, nil
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
