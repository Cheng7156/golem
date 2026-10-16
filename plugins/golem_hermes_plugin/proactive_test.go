package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeEngine 引擎桩：记录请求、返回固定文本
type fakeEngine struct {
	text string
	err  error
	reqs []runRequest
}

func (e *fakeEngine) Run(ctx context.Context, req runRequest, onEvent func(runEvent)) (engineResult, error) {
	return engineResult{Text: e.text}, e.err
}
func (e *fakeEngine) Complete(ctx context.Context, req runRequest) (engineResult, error) {
	e.reqs = append(e.reqs, req)
	return engineResult{Text: e.text}, e.err
}
func (e *fakeEngine) Health(ctx context.Context) error { return nil }

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// enablePro 开启测试群的冷场看护（threshold 分钟）
func enablePro(t *testing.T, p *hermesPlugin, threshold int) {
	t.Helper()
	if err := p.store.saveChatSettings(chatSettings{
		Chat: "g@chatroom", ProactiveEnabled: boolPtr(true), ProactiveMin: intPtr(threshold),
	}); err != nil {
		t.Fatalf("保存群设置失败: %v", err)
	}
}

// proWaitSpin 等异步 fire 落地（send/reqs/状态由调用方谓词判断）
func proWaitSpin(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待异步冒泡超时")
}

func TestProJitteredThreshold(t *testing.T) {
	now := time.Now()
	for _, base := range []int{40, 5, 1, 120} {
		lo, hi := time.Duration(base*75)*time.Minute/100, time.Duration(base*125)*time.Minute/100
		if base < 5 {
			lo, hi = 225*time.Second, 375*time.Second // 下限钳到 5 分钟 ±25%
		}
		for _, chat := range []string{"g@a", "g@b", "g@c"} {
			got := proJitteredThreshold(chat, now, base)
			if got < lo || got > hi {
				t.Fatalf("base=%d chat=%s 抖动越界: %v 不在 [%v,%v]", base, chat, got, lo, hi)
			}
			if again := proJitteredThreshold(chat, now, base); again != got {
				t.Fatalf("同群同日应确定一致: %v vs %v", got, again)
			}
		}
	}
}

func TestInQuietHours(t *testing.T) {
	mk := func(h int) time.Time {
		return time.Date(2026, 10, 16, h, 30, 0, 0, time.Local)
	}
	// 常规 1-8
	if !inQuietHours(mk(3), 1, 8) {
		t.Fatal("凌晨3点应在静默时段")
	}
	if inQuietHours(mk(10), 1, 8) {
		t.Fatal("上午10点不应在静默时段")
	}
	if inQuietHours(mk(0), 1, 8) {
		t.Fatal("0点不在 [1,8) 内")
	}
	// 跨夜 23-7
	if !inQuietHours(mk(23), 23, 7) {
		t.Fatal("跨夜时段 23 点应在静默")
	}
	if !inQuietHours(mk(3), 23, 7) {
		t.Fatal("跨夜时段凌晨3点应在静默")
	}
	if inQuietHours(mk(12), 23, 7) {
		t.Fatal("中午不应在跨夜静默时段")
	}
	// start==end 恒不静默
	if inQuietHours(mk(3), 5, 5) {
		t.Fatal("start==end 应不静默")
	}
}

func TestProTruncate(t *testing.T) {
	if got := proTruncate("短句", 80); got != "短句" {
		t.Fatalf("短文本不应截断: %q", got)
	}
	long := strings.Repeat("啊", 100)
	got := []rune(proTruncate(long, 80))
	if len(got) > 80 {
		t.Fatalf("截断后超长: %d", len(got))
	}
	// 有句读时优先在句读收刀
	withDot := strings.Repeat("哈", 50) + "。" + strings.Repeat("嘿", 60)
	if got := proTruncate(withDot, 80); !strings.HasSuffix(got, "。") || len([]rune(got)) > 80 {
		t.Fatalf("应在句读处收刀: %q", got)
	}
}

func TestSessionBucketAt(t *testing.T) {
	p := &hermesPlugin{}
	p.Config.SessionRotateMin = 60
	if got := p.sessionBucketAt(3600); got != ":1" {
		t.Fatalf("3600s 应落在 1 号桶，得 %q", got)
	}
	if got := p.sessionBucketAt(3599); got != ":0" {
		t.Fatalf("3599s 应落在 0 号桶，得 %q", got)
	}
	// 沉默跨桶：回的是沉默开始时的桶，不是现在的桶
	p.Config.SessionRotateMin = 60
	lastHuman := time.Now().Add(-50 * time.Minute).Unix()
	key := "wx:group:g@chatroom" + p.sessionBucketAt(lastHuman)
	if strings.HasSuffix(key, p.sessionBucket()) && lastHuman/3600 != time.Now().Unix()/3600 {
		t.Fatalf("跨桶沉默应用旧桶钥匙: %s", key)
	}
	p.Config.SessionRotateMin = 0
	if got := p.sessionBucketAt(time.Now().Unix()); got != "" {
		t.Fatalf("rotate=0 应无后缀，得 %q", got)
	}
}

// TestProactiveFireE2E 全链路：门通过 → 带旧桶会话钥匙生成 → 硬检 → 发送 → 记账
func TestProactiveFireE2E(t *testing.T) {
	p, fake := newFunTestPlugin(t)
	eng := &fakeEngine{text: "话说刚才那个事情后来怎么样了（笑）"}
	p.eng = eng
	p.Config.FastProfile = "fast"
	p.Config.SessionRotateMin = 60
	p.Config.ProactiveDailyCap = 5
	enablePro(t, p, 40)

	// 沉默 50 分钟，跨过了桶边界
	lastHuman := time.Now().Add(-50 * time.Minute)
	p.fun.noteHuman("g@chatroom", lastHuman)

	now := time.Now()
	p.fun.proactiveTick(now)
	proWaitSpin(t, func() bool { return len(fake.sends) == 1 })

	// 会话钥匙：沉默发生时的桶
	wantKey := "wx:group:g@chatroom" + p.sessionBucketAt(lastHuman.Unix())
	if eng.reqs[0].SessionKey != wantKey {
		t.Fatalf("会话钥匙应回到沉默时的桶\nwant %q\ngot  %q", wantKey, eng.reqs[0].SessionKey)
	}
	if eng.reqs[0].Profile != "fast" {
		t.Fatalf("应走 fast 通道，得 %q", eng.reqs[0].Profile)
	}
	if !strings.Contains(eng.reqs[0].Prompt, "安静了") || !strings.Contains(eng.reqs[0].Prompt, "分钟") {
		t.Fatalf("prompt 缺沉默时长元数据:\n%s", eng.reqs[0].Prompt)
	}
	// 发送内容：括号小动作被剥掉
	if strings.Contains(fake.sends[0].GetContent(), "（笑）") {
		t.Fatalf("括号小动作应被剥除: %q", fake.sends[0].GetContent())
	}
	// 记账：日上限 kv、streak、bot 时钟、复读底稿
	today := now.Format("2006-01-02")
	if kv := p.store.kvGet("procap:g@chatroom:" + today); kv != "1" {
		t.Fatalf("日计数 kv 应为 1，得 %q", kv)
	}
	if p.fun.state.sil.proStreak("g@chatroom") != 1 {
		t.Fatal("streak 应为 1")
	}
	p.fun.state.sil.mu.Lock()
	lastBot := p.fun.state.sil.lastBot["g@chatroom"]
	texts := append([]string(nil), p.fun.state.sil.botTexts["g@chatroom"]...)
	p.fun.state.sil.mu.Unlock()
	if lastBot == 0 || len(texts) != 1 {
		t.Fatalf("bot 时钟/复读底稿未记账: lastBot=%d texts=%v", lastBot, texts)
	}
	// firing 已解除，二次 tick 在 bot 冷却期内不应再发
	p.fun.state.sil.mu.Lock()
	busy := p.fun.state.sil.firing["g@chatroom"]
	p.fun.state.sil.mu.Unlock()
	if busy {
		t.Fatal("firing 应已解除")
	}
	p.fun.proactiveTick(time.Now())
	time.Sleep(80 * time.Millisecond)
	if len(fake.sends) != 1 || len(eng.reqs) != 1 {
		t.Fatalf("bot 冷却期内不应再发: sends=%d reqs=%d", len(fake.sends), len(eng.reqs))
	}
}

// TestProactiveGates 逐道闸门：命中任一硬闸绝不派发生成
func TestProactiveGates(t *testing.T) {
	mk := func(tweak func(p *hermesPlugin)) (*hermesPlugin, *fakeEngine) {
		p, _ := newFunTestPlugin(t)
		eng := &fakeEngine{text: "冒个泡"}
		p.eng = eng
		p.Config.FastProfile = "fast"
		p.Config.ProactiveDailyCap = 2
		enablePro(t, p, 40)
		p.fun.noteHuman("g@chatroom", time.Now().Add(-50*time.Minute))
		if tweak != nil {
			tweak(p)
		}
		return p, eng
	}
	quiet := func() time.Time { return time.Date(2026, 10, 16, 4, 0, 0, 0, time.Local) }

	cases := []struct {
		name  string
		tweak func(p *hermesPlugin)
		now   time.Time
	}{
		{"沉默不足", func(p *hermesPlugin) {
			p.fun.noteHuman("g@chatroom", time.Now().Add(-10*time.Minute))
		}, time.Now()},
		{"静默时段", nil, quiet()},
		{"bot刚开过口", func(p *hermesPlugin) {
			p.fun.noteBot("g@chatroom", "刚说过的", time.Now().Add(-2*time.Minute))
		}, time.Now()},
		{"streak到顶", func(p *hermesPlugin) {
			p.fun.state.sil.mu.Lock()
			p.fun.state.sil.streak["g@chatroom"] = proMaxStreak
			p.fun.state.sil.mu.Unlock()
		}, time.Now()},
		{"连败禁用", func(p *hermesPlugin) {
			p.fun.state.sil.mu.Lock()
			p.fun.state.sil.fails["g@chatroom"] = proFailGiveup
			p.fun.state.sil.mu.Unlock()
		}, time.Now()},
		{"退避中", func(p *hermesPlugin) {
			p.fun.state.sil.mu.Lock()
			p.fun.state.sil.nextTry["g@chatroom"] = time.Now().Add(20 * time.Minute).Unix()
			p.fun.state.sil.mu.Unlock()
		}, time.Now()},
		{"日上限用尽", func(p *hermesPlugin) {
			p.store.kvSet("procap:g@chatroom:"+time.Now().Format("2006-01-02"), "2")
		}, time.Now()},
		{"群级关闭", func(p *hermesPlugin) {
			_ = p.store.saveChatSettings(chatSettings{Chat: "g@chatroom", ProactiveEnabled: boolPtr(false)})
		}, time.Now()},
	}
	for _, c := range cases {
		p, eng := mk(c.tweak)
		p.fun.proactiveTick(c.now)
		time.Sleep(80 * time.Millisecond)
		if len(eng.reqs) != 0 {
			t.Fatalf("%s: 不应派发生成，却调了 %d 次", c.name, len(eng.reqs))
		}
		p.fun.state.sil.mu.Lock()
		busy := p.fun.state.sil.firing["g@chatroom"]
		p.fun.state.sil.mu.Unlock()
		if busy {
			t.Fatalf("%s: 不应置 firing", c.name)
		}
	}
	// 阈值覆盖生效：群级 5 分钟 < 沉默 8 分钟 → 应放行
	p, eng := mk(func(p *hermesPlugin) {
		_ = p.store.saveChatSettings(chatSettings{Chat: "g@chatroom", ProactiveEnabled: boolPtr(true), ProactiveMin: intPtr(5)})
		p.fun.noteHuman("g@chatroom", time.Now().Add(-8*time.Minute))
	})
	p.fun.proactiveTick(time.Now())
	proWaitSpin(t, func() bool { return len(eng.reqs) > 0 })
}

// TestProactiveSkipAndReject agent 内容否决与复读/失败退避
func TestProactiveSkipAndReject(t *testing.T) {
	// [skip]：不发送、不算失败、不耗日上限，但退避
	p, fake := newFunTestPlugin(t)
	eng := &fakeEngine{text: "[skip]"}
	p.eng = eng
	p.Config.FastProfile = "fast"
	enablePro(t, p, 40)
	p.fun.noteHuman("g@chatroom", time.Now().Add(-50*time.Minute))
	p.fun.proactiveTick(time.Now())
	proWaitSpin(t, func() bool {
		p.fun.state.sil.mu.Lock()
		defer p.fun.state.sil.mu.Unlock()
		return p.fun.state.sil.nextTry["g@chatroom"] > 0
	})
	if len(fake.sends) != 0 {
		t.Fatalf("skip 不应发送: %v", fake.sends)
	}
	p.fun.state.sil.mu.Lock()
	fails := p.fun.state.sil.fails["g@chatroom"]
	p.fun.state.sil.mu.Unlock()
	if fails != 0 {
		t.Fatalf("skip 不应计失败，得 %d", fails)
	}
	if kv := p.store.kvGet("procap:g@chatroom:" + time.Now().Format("2006-01-02")); kv != "" {
		t.Fatalf("skip 不应耗日上限，kv=%q", kv)
	}

	// 复读：agent 的话与自己刚说过的高度相似 → 拒发 + 计失败
	p2, fake2 := newFunTestPlugin(t)
	eng2 := &fakeEngine{text: "今天真的是累坏了呀朋友们"}
	p2.eng = eng2
	p2.Config.FastProfile = "fast"
	enablePro(t, p2, 40)
	p2.fun.noteBot("g@chatroom", "今天真的是累坏了呀朋友们", time.Now().Add(-20*time.Minute))
	p2.fun.noteHuman("g@chatroom", time.Now().Add(-50*time.Minute))
	p2.fun.proactiveTick(time.Now())
	proWaitSpin(t, func() bool {
		p2.fun.state.sil.mu.Lock()
		defer p2.fun.state.sil.mu.Unlock()
		return p2.fun.state.sil.fails["g@chatroom"] > 0
	})
	if len(fake2.sends) != 0 {
		t.Fatalf("复读应被拒发: %v", fake2.sends)
	}

	// 生成失败 → 退避；连败 3 次禁用
	p3, _ := newFunTestPlugin(t)
	eng3 := &fakeEngine{err: context.DeadlineExceeded}
	p3.eng = eng3
	p3.Config.FastProfile = "fast"
	enablePro(t, p3, 40)
	p3.fun.noteHuman("g@chatroom", time.Now().Add(-50*time.Minute))
	for i := 0; i < proFailGiveup; i++ {
		p3.fun.state.sil.mu.Lock()
		p3.fun.state.sil.nextTry["g@chatroom"] = 0
		p3.fun.state.sil.mu.Unlock()
		p3.fun.proactiveTick(time.Now())
		proWaitSpin(t, func() bool {
			p3.fun.state.sil.mu.Lock()
			defer p3.fun.state.sil.mu.Unlock()
			return p3.fun.state.sil.fails["g@chatroom"] > i
		})
	}
	// 第 4 次 tick：连败到顶，不再派发
	eng3.err = nil
	p3.fun.proactiveTick(time.Now())
	time.Sleep(80 * time.Millisecond)
	if len(eng3.reqs) != proFailGiveup {
		t.Fatalf("连败后应禁用，实际又调了引擎: %d 次", len(eng3.reqs))
	}
	// 真人说话解禁
	p3.fun.noteHuman("g@chatroom", time.Now())
	// 沉默不足（刚说话），不会立刻发，但 fails 已清零
	p3.fun.state.sil.mu.Lock()
	f3 := p3.fun.state.sil.fails["g@chatroom"]
	p3.fun.state.sil.mu.Unlock()
	if f3 != 0 {
		t.Fatalf("真人说话应清零 fails，得 %d", f3)
	}
}

// TestProSeedFromArchive reload 后从归档播种真人时钟
func TestProSeedFromArchive(t *testing.T) {
	p, _ := newFunTestPlugin(t)
	old := time.Now().Add(-2 * time.Hour).Unix()
	p.store.mu.Lock()
	p.store.db.Exec("INSERT INTO msg_archive(ts,chat,wxid,name,kind,content) VALUES(?,?,?,?,?,?)",
		old, "g@chatroom", "wxid_x", "张三", "text", " hello ")
	p.store.mu.Unlock()
	p.fun.proSeed("g@chatroom", time.Now())
	p.fun.state.sil.mu.Lock()
	got := p.fun.state.sil.lastHuman["g@chatroom"]
	p.fun.state.sil.mu.Unlock()
	if got != old {
		t.Fatalf("应从归档播种 lastHuman=%d，得 %d", old, got)
	}
	// 无归档的群：保持 0（等首条真人消息再武装）
	p.fun.proSeed("g@other", time.Now())
	p.fun.state.sil.mu.Lock()
	got2 := p.fun.state.sil.lastHuman["g@other"]
	p.fun.state.sil.mu.Unlock()
	if got2 != 0 {
		t.Fatalf("无归档应为 0，得 %d", got2)
	}
}

// TestProKvBaseSurvivesReload 日计数以 kv 为权威：内存清零后跨日计数从 kv 起步
func TestProKvBaseSurvivesReload(t *testing.T) {
	p, _ := newFunTestPlugin(t)
	today := time.Now().Format("2006-01-02")
	// 模拟 reload 后 kv 里已有 1 次账
	n := p.fun.state.sil.proBump("g@chatroom", today, 1)
	if n != 2 {
		t.Fatalf("kv 基准 1 上再发应记 2，得 %d", n)
	}
	// 跨日：昨日内存计数作废，从今日 kv 起步
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	n2 := p.fun.state.sil.proBump("g@chatroom", tomorrow, 0)
	if n2 != 1 {
		t.Fatalf("跨日应从 kv 基准 0 起步，得 %d", n2)
	}
}

// TestProPersonaInjected 群级人格面具应注入冒泡 prompt
func TestProPersonaInjected(t *testing.T) {
	p, _ := newFunTestPlugin(t)
	p.store.initPersonas()
	pa := personaPresets[1] // 非内置人格
	if err := p.store.saveChatSettings(chatSettings{Chat: "g@chatroom", Persona: &pa.Name}); err != nil {
		t.Fatalf("保存人格失败: %v", err)
	}
	f := p.fun
	got := f.proPrompt("g@chatroom", time.Now().Add(-45*time.Minute).Unix(), time.Now())
	if !strings.Contains(got, "人格面具") || !strings.Contains(got, pa.Name) {
		t.Fatalf("prompt 应注入人格 %s:\n%s", pa.Name, got)
	}
	// 内置人格不出面具块
	_ = p.store.saveChatSettings(chatSettings{Chat: "g@chatroom"})
	got2 := f.proPrompt("g@chatroom", time.Now().Add(-45*time.Minute).Unix(), time.Now())
	if strings.Contains(got2, "人格面具") {
		t.Fatalf("内置人格不应出面具块:\n%s", got2)
	}
}
