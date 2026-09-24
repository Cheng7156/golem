package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newProfileStore(t *testing.T) *store {
	t.Helper()
	dir := t.TempDir()
	s := newStore(filepath.Join(dir, "state.db"), filepath.Join(dir, "files"))
	t.Cleanup(s.close)
	return s
}

func TestArchiveAndCandidates(t *testing.T) {
	s := newProfileStore(t)
	base := time.Now().Unix()
	for i := 0; i < 25; i++ {
		s.archive(archiveMsg{Ts: base, Chat: "g@chatroom", Wxid: "wxid_a", Name: "张三", Kind: "text", Content: "绝了绝了"})
	}
	for i := 0; i < 5; i++ {
		s.archive(archiveMsg{Ts: base, Chat: "g@chatroom", Wxid: "wxid_b", Name: "李四", Kind: "text", Content: "哈哈"})
	}
	time.Sleep(1500 * time.Millisecond) // 等 flush 周期
	cands := s.profileCandidates(20, 10)
	if len(cands) != 1 || cands[0].Wxid != "wxid_a" {
		t.Fatalf("候选应为 wxid_a（25条），实际 %+v", cands)
	}
	if cands[0].Count != 25 {
		t.Fatalf("计数应为25，实际 %d", cands[0].Count)
	}
	corpus := s.memberCorpus("wxid_a", 0, cands[0].MaxID, 200)
	if len(corpus) != 25 {
		t.Fatalf("语料应为25条，实际 %d", len(corpus))
	}
}

func TestMemberMergeAndSummary(t *testing.T) {
	s := newProfileStore(t)
	now := time.Now().Unix()
	r := &memberRow{Wxid: "wxid_a", Name: "张三", Summary: "电竞老哥",
		Facts: []memberFact{{Kind: "喜好", Content: "玩原神", At: now - 100, Source: "后台抽取"}}}
	r.Facts = mergeFacts(r.Facts, []memberFact{
		{Kind: "喜好", Content: "玩原神每天三小时", Source: "后台抽取"}, // 前12字去重应命中"玩原神"
		{Kind: "flag", Content: "说要戒奶茶", Source: "对话记住"},
	}, now)
	if len(r.Facts) != 2 {
		t.Fatalf("去重后应剩2条，实际 %d: %+v", len(r.Facts), r.Facts)
	}
	r.UpdatedAt = now
	s.memberSave(r)

	got := s.memberGet("wxid_a")
	if got == nil || got.Summary != "电竞老哥" || len(got.Facts) != 2 {
		t.Fatalf("读取画像异常: %+v", got)
	}
	sum := s.memberSummary("wxid_a")
	if sum == "" || !containsStr([]string{sum}, sum) {
		t.Fatalf("summary 异常: %q", sum)
	}
	t.Logf("summary: %s", sum)
}

func TestMemberRememberAndCards(t *testing.T) {
	s := newProfileStore(t)
	if !s.memberRemember("wxid_c", "王五", "flag", "周五请李四吃蛋挞", "群聊") {
		t.Fatal("remember 应成功")
	}
	if !s.memberRemember("wxid_c", "王五", "无效类别", "测试默认类别", "群聊") {
		t.Fatal("remember 非法类别应回退成功")
	}
	r := s.memberGet("wxid_c")
	if len(r.Facts) != 2 {
		t.Fatalf("应有2条记忆，实际 %d", len(r.Facts))
	}
	if r.Facts[1].Kind != "身份" {
		t.Fatalf("非法类别应回退为身份，实际 %s", r.Facts[1].Kind)
	}
	if r.Facts[0].Source != "对话记住" {
		t.Fatalf("来源应为对话记住，实际 %s", r.Facts[0].Source)
	}
	base := time.Now().Unix()
	for i := 0; i < 10; i++ {
		s.archive(archiveMsg{Ts: base, Chat: "g", Wxid: "wxid_c", Name: "王五", Kind: "text", Content: "x"})
	}
	time.Sleep(1200 * time.Millisecond)
	cards := s.memberCards()
	if len(cards) != 1 || cards[0].Name != "王五" || cards[0].Msg7d != 10 {
		t.Fatalf("卡片异常: %+v", cards)
	}
	d := s.memberDetailGet("wxid_c", []string{"开心", "无语"})
	if d == nil || len(d.Facts) != 2 {
		t.Fatalf("详情异常: %+v", d)
	}
	sum := 0
	for _, h := range d.Hours {
		sum += h
	}
	if sum != 10 {
		t.Fatalf("小时分布总和应为10，实际 %d", sum)
	}
}

func TestSafeWxid(t *testing.T) {
	for _, ok := range []string{"wxid_abc123", "12345@chatroom", "gh_-x"} {
		if !safeWxid(ok) {
			t.Fatalf("%s 应合法", ok)
		}
	}
	for _, bad := range []string{"", "a b", "x/y", "a\\b", "../../etc"} {
		if safeWxid(bad) {
			t.Fatalf("%s 应非法", bad)
		}
	}
}
