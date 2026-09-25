package main

import (
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
