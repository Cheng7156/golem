package main

import (
	"database/sql"
	"errors"
	"log/slog"
	"strings"
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
	CrowEnabled       *bool    `json:"crow_enabled"`        // 打鸣
	GrudgeEnabled     *bool    `json:"grudge_enabled"`      // 记仇小本本
	MoodEnabled       *bool    `json:"mood_enabled"`        // 突然的自我
	ImagegenEnabled   *bool    `json:"imagegen_enabled"`    // 画坊（本群接单）
	ImagegenDailyCap  *int     `json:"imagegen_daily_cap"`  // 本群日产能
	ProactiveEnabled  *bool    `json:"proactive_enabled"`   // 冷场看护（本群主动冒泡）
	ProactiveMin      *int     `json:"proactive_min"`       // 冷场阈值分钟数（nil/0=跟随全局）
	Persona           *string  `json:"persona"`             // 人格面具（人格名；nil=跟随全局默认）
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
		chronicle INTEGER, session_mode TEXT, updated_at INTEGER,
		crow_enabled INTEGER, grudge_enabled INTEGER, mood_enabled INTEGER,
		imagegen_enabled INTEGER, imagegen_daily_cap INTEGER,
		proactive_enabled INTEGER, proactive_min INTEGER)`)
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS member_flags(
		wxid TEXT PRIMARY KEY, owner INTEGER DEFAULT 0, ignored INTEGER DEFAULT 0, updated_at INTEGER)`)
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS grudges(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER, chat TEXT, wxid TEXT, name TEXT, reason TEXT, msg_id INTEGER DEFAULT 0, dug INTEGER DEFAULT 0)`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_grudges_chat ON grudges(chat, dug)`)
	// 旧库迁移：CREATE IF NOT EXISTS 对已存在的老表不生效，逐列补齐（列已存在时报错忽略）。
	// 血泪教训：v0.7.0 给这张表加列时漏了迁移，运行库缺列导致群设置读写全部静默失败。
	for _, col := range []string{
		"mute INTEGER", "free_response INTEGER", "ambient_rate REAL", "ambient_cooldown_sec INTEGER",
		"chronicle INTEGER", "session_mode TEXT", "updated_at INTEGER",
		"crow_enabled INTEGER", "grudge_enabled INTEGER", "mood_enabled INTEGER",
		"imagegen_enabled INTEGER", "imagegen_daily_cap INTEGER", "persona TEXT",
		"proactive_enabled INTEGER", "proactive_min INTEGER",
	} {
		if _, err := s.db.Exec("ALTER TABLE chat_settings ADD COLUMN " + col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			slog.Warn("[ghp] chat_settings 迁移异常", "col", col, "err", err)
		}
	}
	// 成员画像表迁移：画像卡缓存两列
	for _, col := range []string{"card_intro TEXT", "card_file TEXT"} {
		if _, err := s.db.Exec("ALTER TABLE member_profiles ADD COLUMN " + col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			slog.Warn("[ghp] member_profiles 迁移异常", "col", col, "err", err)
		}
	}
}

// addGrudge 记一笔仇
func (s *store) addGrudge(g grudge) {
	if s == nil || s.db == nil || g.Wxid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec("INSERT INTO grudges(at,chat,wxid,name,reason,msg_id) VALUES(?,?,?,?,?,?)",
		g.At, g.Chat, g.Wxid, g.Name, g.Reason, g.MsgID)
}

// pickGrudge 挑一条未翻过的旧账（至少隔了 minAge），标记已翻
func (s *store) pickGrudge(chat string, minAge time.Duration) (grudge, bool) {
	g := grudge{}
	if s == nil || s.db == nil {
		return g, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT id,at,wxid,name,reason FROM grudges
		WHERE chat=? AND dug=0 AND at<=? ORDER BY at ASC LIMIT 1`,
		chat, time.Now().Add(-minAge).Unix())
	var id int64
	if err := row.Scan(&id, &g.At, &g.Wxid, &g.Name, &g.Reason); err != nil {
		return g, false
	}
	_, _ = s.db.Exec("UPDATE grudges SET dug=1 WHERE id=?", id)
	return g, true
}

func (s *store) markGrudgeDug(g grudge) {
	if s == nil || s.db == nil || g.Wxid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec("UPDATE grudges SET dug=1 WHERE wxid=? AND at=? AND dug=0", g.Wxid, g.At)
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
	var crow, grudge, mood, imgEnabled, imgCap, proEnabled, proMin sql.NullInt64
	var rate sql.NullFloat64
	var mode, persona sql.NullString
	var updated sql.NullInt64
	err := s.db.QueryRow(`SELECT mute, free_response, ambient_rate, ambient_cooldown_sec, chronicle, session_mode,
		crow_enabled, grudge_enabled, mood_enabled, imagegen_enabled, imagegen_daily_cap, persona, updated_at,
		proactive_enabled, proactive_min
		FROM chat_settings WHERE chat=?`, chat).
		Scan(&mute, &freeResp, &rate, &cooldown, &chronicle, &mode, &crow, &grudge, &mood, &imgEnabled, &imgCap, &persona, &updated, &proEnabled, &proMin)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("[ghp] chat_settings 读取失败（疑似旧库缺列）", "chat", chat, "err", err)
		}
		return cs
	}
	cs.Mute = nullableBool(mute)
	cs.FreeResponse = nullableBool(freeResp)
	cs.AmbientRate = nullableFloat(rate)
	cs.AmbientCooldown = nullableInt(cooldown)
	cs.Chronicle = nullableBool(chronicle)
	cs.SessionMode = nullableStr(mode)
	cs.CrowEnabled = nullableBool(crow)
	cs.GrudgeEnabled = nullableBool(grudge)
	cs.MoodEnabled = nullableBool(mood)
	cs.ImagegenEnabled = nullableBool(imgEnabled)
	cs.ImagegenDailyCap = nullableInt(imgCap)
	cs.ProactiveEnabled = nullableBool(proEnabled)
	cs.ProactiveMin = nullableInt(proMin)
	cs.Persona = nullableStr(persona)
	cs.UpdatedAt = updated.Int64
	return cs
}

// saveChatSettings 保存群级设置（整行覆盖，nil 字段即清除覆盖）。写失败必须上抛——静默丢配置的血案不能再有
func (s *store) saveChatSettings(cs chatSettings) error {
	if s == nil || s.db == nil || cs.Chat == "" {
		return errors.New("store 不可用或 chat 为空")
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
	_, err := s.db.Exec(`INSERT INTO chat_settings(chat,mute,free_response,ambient_rate,ambient_cooldown_sec,chronicle,session_mode,crow_enabled,grudge_enabled,mood_enabled,imagegen_enabled,imagegen_daily_cap,persona,updated_at,proactive_enabled,proactive_min)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(chat) DO UPDATE SET mute=excluded.mute, free_response=excluded.free_response,
		ambient_rate=excluded.ambient_rate, ambient_cooldown_sec=excluded.ambient_cooldown_sec,
		chronicle=excluded.chronicle, session_mode=excluded.session_mode,
		crow_enabled=excluded.crow_enabled, grudge_enabled=excluded.grudge_enabled, mood_enabled=excluded.mood_enabled,
		imagegen_enabled=excluded.imagegen_enabled, imagegen_daily_cap=excluded.imagegen_daily_cap,
		persona=excluded.persona, proactive_enabled=excluded.proactive_enabled, proactive_min=excluded.proactive_min,
		updated_at=excluded.updated_at`,
		cs.Chat, nb(cs.Mute), nb(cs.FreeResponse), nf(cs.AmbientRate), ni(cs.AmbientCooldown),
		nb(cs.Chronicle), ns(cs.SessionMode), nb(cs.CrowEnabled), nb(cs.GrudgeEnabled), nb(cs.MoodEnabled),
		nb(cs.ImagegenEnabled), ni(cs.ImagegenDailyCap), ns(cs.Persona), cs.UpdatedAt,
		nb(cs.ProactiveEnabled), ni(cs.ProactiveMin))
	return err
}

// chatSettingsList 全部有覆盖的群（监控台展示用）
func (s *store) chatSettingsList() []chatSettings {
	out := []chatSettings{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT chat, mute, free_response, ambient_rate, ambient_cooldown_sec, chronicle, session_mode,
		crow_enabled, grudge_enabled, mood_enabled, imagegen_enabled, imagegen_daily_cap, persona, updated_at,
		proactive_enabled, proactive_min
		FROM chat_settings ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cs chatSettings
		var mute, freeResp, cooldown, chronicle, crow, grudge, mood, imgEnabled, imgCap, proEnabled, proMin, updated sql.NullInt64
		var rate sql.NullFloat64
		var mode, persona sql.NullString
		if rows.Scan(&cs.Chat, &mute, &freeResp, &rate, &cooldown, &chronicle, &mode, &crow, &grudge, &mood, &imgEnabled, &imgCap, &persona, &updated, &proEnabled, &proMin) == nil {
			cs.Mute = nullableBool(mute)
			cs.FreeResponse = nullableBool(freeResp)
			cs.AmbientRate = nullableFloat(rate)
			cs.AmbientCooldown = nullableInt(cooldown)
			cs.Chronicle = nullableBool(chronicle)
			cs.SessionMode = nullableStr(mode)
			cs.CrowEnabled = nullableBool(crow)
			cs.GrudgeEnabled = nullableBool(grudge)
			cs.MoodEnabled = nullableBool(mood)
			cs.ImagegenEnabled = nullableBool(imgEnabled)
			cs.ImagegenDailyCap = nullableInt(imgCap)
			cs.Persona = nullableStr(persona)
			cs.ProactiveEnabled = nullableBool(proEnabled)
			cs.ProactiveMin = nullableInt(proMin)
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
	_ = s.db.QueryRow("SELECT owner, ignored, updated_at FROM member_flags WHERE wxid=?", wxid).Scan(&mf.Owner, &mf.Ignored, &mf.UpdatedAt)
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
	CrowEnabled   bool
	GrudgeEnabled bool
	MoodEnabled   bool
	ProactiveEnabled bool
	ProactiveMin     int
	ImagegenEnabled  bool
	ImagegenDailyCap int
}

func (p *hermesPlugin) effectiveChatConfig(groupID string) effectiveChatConfig {
	// 全局默认
	ec := effectiveChatConfig{
		FreeResponse:       p.inFreeList(groupID),
		AmbientRate:        p.Config.AmbientReplyRate,
		AmbientCooldownSec: p.Config.AmbientCooldownSec,
		PerUserSession:     p.Config.GroupSessionsPerUser,
		CrowEnabled:        p.Config.CrowEnabled,
		GrudgeEnabled:      p.Config.GrudgeEnabled,
		MoodEnabled:        p.Config.MoodEnabled,
		ProactiveEnabled:   false, // 冷场看护默认关，仅显式开启（群级或无全局开关）
		ProactiveMin:       p.Config.ProactiveMin,
	}
	// 画坊全局默认来自 kv 运行时配置（Web 可改），群级可覆盖
	ig := p.imagegenSettings()
	ec.ImagegenEnabled = ig.Enabled
	ec.ImagegenDailyCap = ig.DailyCap
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
	if cs.CrowEnabled != nil {
		ec.CrowEnabled = *cs.CrowEnabled
	}
	if cs.GrudgeEnabled != nil {
		ec.GrudgeEnabled = *cs.GrudgeEnabled
	}
	if cs.MoodEnabled != nil {
		ec.MoodEnabled = *cs.MoodEnabled
	}
	if cs.ProactiveEnabled != nil {
		ec.ProactiveEnabled = *cs.ProactiveEnabled
	}
	if cs.ProactiveMin != nil && *cs.ProactiveMin > 0 {
		ec.ProactiveMin = *cs.ProactiveMin
	}
	if cs.ImagegenEnabled != nil {
		ec.ImagegenEnabled = *cs.ImagegenEnabled
	}
	if cs.ImagegenDailyCap != nil && *cs.ImagegenDailyCap > 0 {
		ec.ImagegenDailyCap = *cs.ImagegenDailyCap
	}
	return ec
}
