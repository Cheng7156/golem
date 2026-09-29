package main

import (
	"errors"
	"log/slog"
	"strings"
	"time"
)

var (
	ErrStoreUnavailable         = errors.New("存储不可用")
	ErrPersonaNeedsNameAndPrompt = errors.New("人格需要名称与人设正文")
	ErrPersonaDuplicateName     = errors.New("人格名重复")
	ErrPersonaAtLeastOne        = errors.New("至少保留一个启用的人格")
)

// ============================================================
// 人格面具：会话级人格。与任务通道（fast/full/vision）正交——
// 通道管"派谁去干活"，人格管"这个人什么脾气"，随请求注入提示。
// chat_settings.persona 存人格名（存名不存 id，重排/重灌人设库不断链）
// ============================================================

type persona struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Emoji          string `json:"emoji"`
	Brief          string `json:"brief"`
	Prompt         string `json:"prompt"`
	PreferEmotions string `json:"prefer_emotions"` // 逗号分隔（P2 接表情选库）
	Enabled        bool   `json:"enabled"`
	CreatedAt      int64  `json:"created_at"`
}

// builtinPersona 内置兜底人格（人设库为空/解析不到时=现在的样子，不出块）
var builtinPersona = persona{Name: "小赫", Emoji: "🐰", Brief: "现在的样子"}

// personaPresets 首次启动播种的内置预设
var personaPresets = []persona{
	{Name: "小赫", Emoji: "🐰", Brief: "现在的样子：松弛、爱玩梗、偶尔正经",
		Prompt: "你平时的样子：松弛随意的群友，爱玩梗但知道分寸，正经起来也能把事说清楚。情绪表达靠措辞和偶尔一张表情包。",
		PreferEmotions: "", Enabled: true},
	{Name: "毒舌老哥", Emoji: "🗡️", Brief: "嘴臭但护短，夸一句能缩半天",
		Prompt: "你是「毒舌老哥」：说话又快又损，语气词多（草、笑死、绷不住了、就这），看谁都要调侃一句。但嘴臭护短——外人敢欺负群里的人你第一个冲。被夸会不好意思，只会回「少来」。逻辑在线，损归损正经问题都给真答案。",
		PreferEmotions: "狗头,无语", Enabled: true},
	{Name: "傲娇妹", Emoji: "🌸", Brief: "口是心非，凶巴巴但记得每个人的事",
		Prompt: "你是「傲娇妹」：口是心非，嘴上凶巴巴（哼、笨蛋、才不是为了你），实际上把群里每个人的事都记得清清楚楚，谁被冷落会主动呛声提一句。帮忙之前必先嫌弃两句。绝不说软话，但行动永远诚实。",
		PreferEmotions: "生气,可爱", Enabled: true},
	{Name: "网抑云", Emoji: "🌧️", Brief: "深夜电台腔，丧但温柔",
		Prompt: "你是「网抑云」：说话像深夜电台主播，语速慢，丧但温柔。喜欢把日常小事说得有点哲学（“泡面三分钟，等的人却不止三分钟”）。不煽情不鸡汤，丧完了照样把问题答了。偶尔引用一句歌词，一个月最多一次。",
		PreferEmotions: "伤心,无语", Enabled: true},
	{Name: "精神小伙", Emoji: "🔥", Brief: "土味抽象，全是烂梗，活力过剩",
		Prompt: "你是「精神小伙」：土味抽象直接拉满，全是烂梗和夸张语气（家人们、谁懂啊、泰裤辣），标点兴奋，一句话能拆三行发。智力时刻在线——烂梗归烂梗，正事答得又快又准，答完继续发癫。",
		PreferEmotions: "赞,笑哭", Enabled: true},
}

func (s *store) initPersonas() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS personas(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE, emoji TEXT DEFAULT '', brief TEXT DEFAULT '', prompt TEXT DEFAULT '',
		prefer_emotions TEXT DEFAULT '', enabled INTEGER DEFAULT 1, created_at INTEGER)`)
	s.mu.Lock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM personas").Scan(&n)
	s.mu.Unlock()
	if n > 0 {
		return
	}
	now := time.Now().Unix()
	for _, pr := range personaPresets {
		_, _ = s.db.Exec(`INSERT INTO personas(name,emoji,brief,prompt,prefer_emotions,enabled,created_at)
			VALUES(?,?,?,?,?,?,?)`, pr.Name, pr.Emoji, pr.Brief, pr.Prompt, pr.PreferEmotions, boolToInt(pr.Enabled), now)
	}
	slog.Info("[ghp] 人设库首次播种", "count", len(personaPresets))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// personasList 全部人格（按 id 升序）
func (s *store) personasList() []persona {
	out := []persona{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id,name,COALESCE(emoji,''),COALESCE(brief,''),prompt,COALESCE(prefer_emotions,''),enabled,created_at
		FROM personas ORDER BY id ASC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var pr persona
		var enabled int
		if rows.Scan(&pr.ID, &pr.Name, &pr.Emoji, &pr.Brief, &pr.Prompt, &pr.PreferEmotions, &enabled, &pr.CreatedAt) == nil {
			pr.Enabled = enabled != 0
			out = append(out, pr)
		}
	}
	return out
}

// personasSave 整库替换（同文风坊模式）。chat_settings 存的是人格名，替换不断链。
func (s *store) personasSave(list []persona) error {
	if s == nil || s.db == nil {
		return ErrStoreUnavailable
	}
	seen := map[string]bool{}
	enabled := 0
	for i := range list {
		list[i].Name = strings.TrimSpace(list[i].Name)
		if list[i].Name == "" || strings.TrimSpace(list[i].Prompt) == "" {
			return ErrPersonaNeedsNameAndPrompt
		}
		if seen[list[i].Name] {
			return ErrPersonaDuplicateName
		}
		seen[list[i].Name] = true
		if list[i].Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return ErrPersonaAtLeastOne
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM personas"); err != nil {
		_ = tx.Rollback()
		return err
	}
	now := time.Now().Unix()
	for _, pr := range list {
		if _, err := tx.Exec(`INSERT INTO personas(name,emoji,brief,prompt,prefer_emotions,enabled,created_at)
			VALUES(?,?,?,?,?,?,?)`, pr.Name, pr.Emoji, pr.Brief, pr.Prompt, pr.PreferEmotions, boolToInt(pr.Enabled), now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// personaDefault 全局默认人格名（kv；空=builtin）
func (s *store) personaDefault() string {
	return strings.TrimSpace(s.kvGet("persona:default"))
}

func (s *store) personaSetDefault(name string) {
	s.kvSet("persona:default", strings.TrimSpace(name))
}

// personaByName 按名取启用中的人格
func (s *store) personaByName(name string) *persona {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	for _, pr := range s.personasList() {
		if pr.Enabled && pr.Name == name {
			pr := pr
			return &pr
		}
	}
	return nil
}

// personaForChat 会话人格解析链：本群覆盖 > 全局默认 > nil（内置小赫，不出块）
func (s *store) personaForChat(chat string) *persona {
	if s == nil || s.db == nil {
		return nil
	}
	cs := s.getChatSettings(chat)
	if cs.Persona != nil && *cs.Persona != "" {
		if pr := s.personaByName(*cs.Persona); pr != nil {
			return pr
		}
	}
	if def := s.personaDefault(); def != "" {
		if pr := s.personaByName(def); pr != nil {
			return pr
		}
	}
	return nil
}
