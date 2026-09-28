package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newSettingsStore(t *testing.T) *store {
	t.Helper()
	dir := t.TempDir()
	s := newStore(filepath.Join(dir, "state.db"), filepath.Join(dir, "files"))
	t.Cleanup(s.close)
	return s
}

func TestChatSettingsOverride(t *testing.T) {
	s := newSettingsStore(t)
	// 无记录：全部跟随全局（nil）
	cs := s.getChatSettings("g@chatroom")
	if cs.Mute != nil || cs.FreeResponse != nil || cs.AmbientRate != nil {
		t.Fatalf("空设置应为nil覆盖: %+v", cs)
	}
	// 写入覆盖
	mute, free := true, false
	rate := 0.35
	cooldown := 120
	mode := "per_user"
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", Mute: &mute, FreeResponse: &free,
		AmbientRate: &rate, AmbientCooldown: &cooldown, SessionMode: &mode})
	cs = s.getChatSettings("g@chatroom")
	if cs.Mute == nil || !*cs.Mute || cs.FreeResponse == nil || *cs.FreeResponse {
		t.Fatalf("覆盖读取异常: %+v", cs)
	}
	if *cs.AmbientRate != 0.35 || *cs.AmbientCooldown != 120 || *cs.SessionMode != "per_user" {
		t.Fatalf("数值覆盖异常: %+v", cs)
	}
	// 清除一项覆盖（nil 覆盖）
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", Mute: &mute, AmbientRate: &rate})
	cs = s.getChatSettings("g@chatroom")
	if cs.FreeResponse != nil || cs.SessionMode != nil {
		t.Fatalf("清除覆盖后应为nil: %+v", cs)
	}
	if cs.Mute == nil || !*cs.Mute {
		t.Fatal("未清除项应保留")
	}
	list := s.chatSettingsList()
	if len(list) != 1 || list[0].Chat != "g@chatroom" {
		t.Fatalf("列表异常: %+v", list)
	}
}

func TestMemberFlags(t *testing.T) {
	s := newSettingsStore(t)
	s.saveMemberFlags(memberFlags{Wxid: "wxid_x", Owner: true})
	if !s.isMemberOwner("wxid_x") || s.isMemberIgnored("wxid_x") {
		t.Fatal("owner 标记异常")
	}
	s.saveMemberFlags(memberFlags{Wxid: "wxid_y", Ignored: true})
	if !s.isMemberIgnored("wxid_y") || s.isMemberOwner("wxid_y") {
		t.Fatal("ignored 标记异常")
	}
	// 更新合并（只改一项，另一项保留）
	s.saveMemberFlags(memberFlags{Wxid: "wxid_x", Owner: true, Ignored: true})
	mf := s.getMemberFlags("wxid_x")
	if !mf.Owner || !mf.Ignored {
		t.Fatalf("标记更新异常: %+v", mf)
	}
}

func TestEffectiveChatConfig(t *testing.T) {
	p := newPlugin()
	p.Config = defaultConfig()
	s := newSettingsStore(t)
	p.store = s
	// 全局默认
	ec := p.effectiveChatConfig("g@chatroom")
	if ec.Muted || ec.FreeResponse || ec.AmbientRate != p.Config.AmbientReplyRate {
		t.Fatalf("默认生效值异常: %+v", ec)
	}
	// 群级覆盖
	rate := 0.5
	free := true
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", AmbientRate: &rate, FreeResponse: &free})
	ec = p.effectiveChatConfig("g@chatroom")
	if !ec.FreeResponse || ec.AmbientRate != 0.5 {
		t.Fatalf("群级覆盖未生效: %+v", ec)
	}
	// 其他群不受影响
	ec2 := p.effectiveChatConfig("other@chatroom")
	if ec2.FreeResponse || ec2.AmbientRate != p.Config.AmbientReplyRate {
		t.Fatalf("覆盖泄漏到其他群: %+v", ec2)
	}
}

// TestChatSettingsLegacyMigration 旧库缺列回归：v0.7.0 前建的 chat_settings 没有
// crow/grudge/mood/imagegen 列，曾导致群设置读写全部静默失败（用户改了又"恢复"）。
func TestChatSettingsLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	// 模拟 v0.6.x 时代的旧表（无 crow/grudge/mood/imagegen 列）并塞入老数据
	old, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE chat_settings(
		chat TEXT PRIMARY KEY, mute INTEGER, free_response INTEGER, ambient_rate REAL,
		ambient_cooldown_sec INTEGER, chronicle INTEGER, session_mode TEXT, updated_at INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`INSERT INTO chat_settings VALUES('g@chatroom', NULL, 1, 65.0, NULL, 1, NULL, 100)`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	s := newStore(dbPath, filepath.Join(dir, "files"))
	t.Cleanup(s.close)

	// 老数据必须能读出来
	cs := s.getChatSettings("g@chatroom")
	if cs.FreeResponse == nil || !*cs.FreeResponse || cs.Chronicle == nil || !*cs.Chronicle {
		t.Fatalf("旧数据读取失败: %+v", cs)
	}
	if cs.AmbientRate == nil || *cs.AmbientRate != 65 {
		t.Fatalf("旧 ambient_rate 读取失败: %+v", cs)
	}
	// 新列写入必须成功（此前因缺列静默失败）。
	// 注意保存契约是"整行覆盖"：Web UI 恒为 GET 全量→改→PUT 全量，此处照实模拟。
	crow, imgCap := true, 9
	full := cs // 先取到旧值再整体回写
	full.CrowEnabled = &crow
	full.ImagegenDailyCap = &imgCap
	if err := s.saveChatSettings(full); err != nil {
		t.Fatalf("新列保存失败: %v", err)
	}
	cs = s.getChatSettings("g@chatroom")
	if cs.CrowEnabled == nil || !*cs.CrowEnabled {
		t.Fatalf("crow_enabled 未持久化: %+v", cs)
	}
	if cs.ImagegenDailyCap == nil || *cs.ImagegenDailyCap != 9 {
		t.Fatalf("imagegen_daily_cap 未持久化: %+v", cs)
	}
	// 老数据仍在（未被迁移抹掉）
	if cs.FreeResponse == nil || !*cs.FreeResponse || cs.AmbientRate == nil || *cs.AmbientRate != 65 {
		t.Fatalf("迁移抹掉了旧数据: %+v", cs)
	}
	// 保存错误要上抛（不再静默）
	var empty *store
	if err := empty.saveChatSettings(chatSettings{Chat: "x"}); err == nil {
		t.Fatal("空 store 保存应返回错误")
	}
}
