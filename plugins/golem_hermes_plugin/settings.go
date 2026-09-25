package main

import (
	"database/sql"
	"log/slog"
	"time"
)

// ============================================================
// 配置中枢：群级设置（chat_settings）与成员标记（member_flags）
// 覆盖语义：字段为 nil 表示"跟随全局默认"，Web 行内编辑即时生效
// ============================================================

// chatSettings 群级覆盖（nil = 跟随全局）
type chatSettings struct {
	Chat              string   `json:"chat"`
	Mute              *bool    `json:"mute"`                // 整群静音
	FreeResponse      *bool    `json:"free_response"`       // 免@响应
	AmbientRate       *float64 `json:"ambient_rate"`        // 环境闲聊概率覆盖
	AmbientCooldown   *int     `json:"ambient_cooldown_sec"` // 群冷却覆盖
	Chronicle         *bool    `json:"chronicle"`           // 史官出刊（预留）
	SessionMode       *string  `json:"session_mode"`        // shared | per_user
	UpdatedAt         int64    `json:"updated_at"`
}

// memberFlags 成员标记（主人/屏蔽，Web 可改，即时生效）
type memberFlags struct {
	Wxid     string `json:"wxid"`
	Owner    bool   `json:"owner"`
	Ignored  bool   `json:"ignored"`
	UpdatedAt int64 `json:"updated_at"`
}

func (s *store) initSettings() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS chat_settings(
		chat TEXT PRIMARY KEY,
		mute INTEGER, free_response INTEGER, ambient_rate REAL, ambient_cooldown_sec INTEGER,
		chronicle INTEGER, session_mode TEXT, updated_at INTEGER)`)
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS member_flags(
		wxid TEXT PRIMARY KEY, owner INTEGER DEFAULT 0, ignored INTEGER DEFAULT 0, updated_at INTEGER)`)
}

func nullableBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

func nullableFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func nullableInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func nullableStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// getChatSettings 读取群级设置（无记录则返回空覆盖）
func (s *store) getChatSettings(chat string) chatSettings {
	cs := chatSettings{Chat: chat}
	if s == nil || s.db == nil || chat == "" {
		return cs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var mute, freeResp, cooldown, chronicle sql.NullInt64
	var rate sql.NullFloat64
	var mode sql.NullString
	var updated sql.NullInt64
	err := s.db.QueryRow(`SELECT mute, free_response, ambient_rate, ambient_cooldown_sec, chronicle, session_mode, updated_at
		FROM chat_settings WHERE chat=?`, chat).
		Scan(&mute, &freeResp, &rate, &cooldown, &chronicle, &mode, &updated)
	if err != nil {
		return cs
	}
	cs.Mute = nullableBool(mute)
	cs.FreeResponse = nullableBool(freeResp)
	cs.AmbientRate = nullableFloat(rate)
	cs.AmbientCooldown = nullableInt(cooldown)
	cs.Chronicle = nullableBool(chronicle)
	cs.SessionMode = nullableStr(mode)
	cs.UpdatedAt = updated.Int64
	return cs
}

// saveChatSettings 保存群级设置（整行覆盖，nil 字段即清除覆盖）
func (s *store) saveChatSettings(cs chatSettings) {
	if s == nil || s.db == nil || cs.Chat == "" {
		return
	}
	nb := func(b *bool) any {
		if b == nil {
			return nil
		}
		if *b {
			return 1
		}
		return 0
	}
	nf := func(f *float64) any {
		if f == nil {
			return nil
		}
		return *f
	}
	ni := func(i *int) any {
		if i == nil {
			return nil
		}
		return *i
	}
	ns := func(v *string) any {
		if v == nil {
			return nil
		}
		return *v
	}
	cs.UpdatedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO chat_settings(chat,mute,free_response,ambient_rate,ambient_cooldown_sec,chronicle,session_mode,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(chat) DO UPDATE SET mute=excluded.mute, free_response=excluded.free_response,
		ambient_rate=excluded.ambient_rate, ambient_cooldown_sec=excluded.ambient_cooldown_sec,
		chronicle=excluded.chronicle, session_mode=excluded.session_mode, updated_at=excluded.updated_at`,
		cs.Chat, nb(cs.Mute), nb(cs.FreeResponse), nf(cs.AmbientRate), ni(cs.AmbientCooldown),
		nb(cs.Chronicle), ns(cs.SessionMode), cs.UpdatedAt)
}

// chatSettingsList 全部有覆盖的群（监控台展示用）
func (s *store) chatSettingsList() []chatSettings {
	out := []chatSettings{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT chat, mute, free_response, ambient_rate, ambient_cooldown_sec, chronicle, session_mode, updated_at
		FROM chat_settings ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cs chatSettings
		var mute, freeResp, cooldown, chronicle, updated sql.NullInt64
		var rate sql.NullFloat64
		var mode sql.NullString
		if rows.Scan(&cs.Chat, &mute, &freeResp, &rate, &cooldown, &chronicle, &mode, &updated) == nil {
			cs.Mute = nullableBool(mute)
			cs.FreeResponse = nullableBool(freeResp)
			cs.AmbientRate = nullableFloat(rate)
			cs.AmbientCooldown = nullableInt(cooldown)
			cs.Chronicle = nullableBool(chronicle)
			cs.SessionMode = nullableStr(mode)
			cs.UpdatedAt = updated.Int64
			out = append(out, cs)
		}
	}
	return out
}

// ---- 成员标记 ----

func (s *store) getMemberFlags(wxid string) memberFlags {
	mf := memberFlags{Wxid: wxid}
	if s == nil || s.db == nil || wxid == "" {
		return mf
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.db.QueryRow("SELECT owner, ignored FROM member_flags WHERE wxid=?", wxid).Scan(&mf.Owner, &mf.Ignored)
	return mf
}

func (s *store) saveMemberFlags(mf memberFlags) {
	if s == nil || s.db == nil || mf.Wxid == "" {
		return
	}
	o, ig := 0, 0
	if mf.Owner {
		o = 1
	}
	if mf.Ignored {
		ig = 1
	}
	mf.UpdatedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO member_flags(wxid,owner,ignored,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(wxid) DO UPDATE SET owner=excluded.owner, ignored=excluded.ignored, updated_at=excluded.updated_at`,
		mf.Wxid, o, ig, mf.UpdatedAt)
	slog.Info("[ghp] 成员标记更新", "wxid", mf.Wxid, "owner", o == 1, "ignored", ig == 1)
}

func (s *store) isMemberIgnored(wxid string) bool {
	return s.getMemberFlags(wxid).Ignored
}

func (s *store) isMemberOwner(wxid string) bool {
	return s.getMemberFlags(wxid).Owner
}

// effectiveChatConfig 决策链取生效值：群级覆盖 > 全局默认
type effectiveChatConfig struct {
	Muted         bool
	FreeResponse  bool
	AmbientRate   float64
	AmbientCooldownSec int
	Chronicle     bool
	PerUserSession bool
}

func (p *hermesPlugin) effectiveChatConfig(groupID string) effectiveChatConfig {
	// 全局默认
	ec := effectiveChatConfig{
		FreeResponse:       p.inFreeList(groupID),
		AmbientRate:        p.Config.AmbientReplyRate,
		AmbientCooldownSec: p.Config.AmbientCooldownSec,
		PerUserSession:     p.Config.GroupSessionsPerUser,
	}
	cs := p.store.getChatSettings(groupID)
	if cs.Mute != nil {
		ec.Muted = *cs.Mute
	}
	if cs.FreeResponse != nil {
		ec.FreeResponse = *cs.FreeResponse
	}
	if cs.AmbientRate != nil {
		ec.AmbientRate = *cs.AmbientRate
	}
	if cs.AmbientCooldown != nil {
		ec.AmbientCooldownSec = *cs.AmbientCooldown
	}
	if cs.Chronicle != nil {
		ec.Chronicle = *cs.Chronicle
	}
	if cs.SessionMode != nil {
		ec.PerUserSession = *cs.SessionMode == "per_user"
	}
	return ec
}
