package main

import (
	"strings"
	"testing"
)

func TestPersonaSeedAndCRUD(t *testing.T) {
	s := newTestStore(t)
	// 首次播种 5 预设
	seeded := s.personasList()
	if len(seeded) != len(personaPresets) {
		t.Fatalf("预设应播种 %d 个，得 %d", len(personaPresets), len(seeded))
	}
	// 整库替换：改一个 + 删到只剩两个
	kept := []persona{
		{Name: "毒舌老哥", Emoji: "🗡️", Prompt: "嘴臭护短", Enabled: true},
		{Name: "傲娇妹", Emoji: "🌸", Prompt: "口是心非", Enabled: false},
	}
	if err := s.personasSave(kept); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if n := len(s.personasList()); n != 2 {
		t.Fatalf("整库替换后应 2 个，得 %d", n)
	}
	// 校验规则
	if err := s.personasSave([]persona{{Name: "", Prompt: "x", Enabled: true}}); err == nil {
		t.Fatal("空名应拒绝")
	}
	if err := s.personasSave([]persona{{Name: "A", Prompt: "x", Enabled: true}, {Name: "A", Prompt: "y", Enabled: true}}); err == nil {
		t.Fatal("重名应拒绝")
	}
	if err := s.personasSave([]persona{{Name: "A", Prompt: "x", Enabled: false}}); err == nil {
		t.Fatal("全停用应拒绝")
	}
}

func TestPersonaResolutionChain(t *testing.T) {
	s := newTestStore(t)
	p := &hermesPlugin{store: s}
	// 1. 无覆盖无默认 → nil（内置，不出块）
	if pr := p.store.personaForChat("g@chatroom"); pr != nil {
		t.Fatalf("无覆盖应 nil，得 %+v", pr)
	}
	// 2. 全局默认
	s.personaSetDefault("毒舌老哥")
	if pr := s.personaForChat("g@chatroom"); pr == nil || pr.Name != "毒舌老哥" {
		t.Fatalf("应解析到全局默认毒舌老哥，得 %+v", pr)
	}
	// 3. 会话覆盖 > 全局
	over := "傲娇妹"
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", Persona: &over})
	if pr := s.personaForChat("g@chatroom"); pr == nil || pr.Name != "傲娇妹" {
		t.Fatalf("会话覆盖应优先，得 %+v", pr)
	}
	// 4. 指向不存在的人格名 → 回落默认
	ghost := "不存在的人"
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", Persona: &ghost})
	if pr := s.personaForChat("g@chatroom"); pr == nil || pr.Name != "毒舌老哥" {
		t.Fatalf("幽灵名应回落全局默认，得 %+v", pr)
	}
}

func TestBuildPromptPersonaInjection(t *testing.T) {
	s := newTestStore(t)
	p := &hermesPlugin{store: s}
	p.metrics = newMetrics()
	p.rings = newRingStore(10)
	d := newDispatcher(&p.Config, nil, nil, p.rings, s, p.metrics)

	tk := &task{Receiver: "g@chatroom", GroupID: "g@chatroom", IsGroup: true, Speaker: "张三", SpeakerID: "wxid_z", Text: "在吗"}
	// 内置人格：不出块
	if out := d.buildPrompt(tk); strings.Contains(out, "人格面具") {
		t.Fatal("内置人格不应注入人格块")
	}
	// 会话覆盖毒舌：出块且含关键词
	over := "毒舌老哥"
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", Persona: &over})
	out := d.buildPrompt(tk)
	if !strings.Contains(out, "[人格面具") || !strings.Contains(out, "毒舌老哥") || !strings.Contains(out, "嘴臭") {
		t.Fatalf("人格块未注入: %s", out[:min(len(out), 300)])
	}
	// 身份信封仍在（人格块不顶掉信封）
	if !strings.Contains(out, "[身份信封") {
		t.Fatal("身份信封丢失")
	}
}
