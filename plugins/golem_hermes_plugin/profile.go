package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 群成员画像（L3 记忆）：消息归档 → 后台抽取 → prompt 注入 / Web 展示
// 数据全部落插件自己的 SQLite（state.db），与表情库同库
// ============================================================

// memberFact 单条画像事实
type memberFact struct {
	Kind    string `json:"kind"`    // 口头禅|喜好|身份|关系|flag|名场面
	Content string `json:"content"`
	At      int64  `json:"at"`
	Source  string `json:"source"` // 后台抽取 | 对话记住
	Chat    string `json:"chat,omitempty"`
	Quote   string `json:"quote,omitempty"` // 依据原句
}

var factKinds = map[string]bool{
	"口头禅": true, "喜好": true, "身份": true, "关系": true, "flag": true, "名场面": true,
}

// archiveMsg 归档消息（异步写入）
type archiveMsg struct {
	Ts      int64
	Chat    string
	Wxid    string
	Name    string
	Kind    string
	Content string
}

// ---- 归档表（store 内嵌异步写入器） ----

func (s *store) startArchive() {
	s.archCh = make(chan archiveMsg, 512)
	s.archDone = make(chan struct{})
	go func() {
		defer close(s.archDone)
		buf := make([]archiveMsg, 0, 64)
		flush := func() {
			if len(buf) == 0 {
				return
			}
			s.mu.Lock()
			tx, err := s.db.Begin()
			if err == nil {
				stmt, _ := tx.Prepare("INSERT INTO msg_archive(ts,chat,wxid,name,kind,content) VALUES(?,?,?,?,?,?)")
				if stmt != nil {
					for _, m := range buf {
						_, _ = stmt.Exec(m.Ts, m.Chat, m.Wxid, m.Name, m.Kind, m.Content)
					}
					stmt.Close()
				}
				_ = tx.Commit()
			}
			s.mu.Unlock()
			buf = buf[:0]
		}
		tick := time.NewTicker(700 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case m, ok := <-s.archCh:
				if !ok {
					flush()
					return
				}
				buf = append(buf, m)
				if len(buf) >= 64 {
					flush()
				}
			case <-tick.C:
				flush()
			}
		}
	}()
}

// archive 非阻塞投递；满则丢弃（消息主链路绝不被画像拖住）
func (s *store) archive(m archiveMsg) {
	if s == nil || s.archCh == nil {
		return
	}
	select {
	case s.archCh <- m:
	default:
	}
}

func (s *store) stopArchive() {
	if s.archCh != nil {
		close(s.archCh)
		<-s.archDone
		s.archCh = nil
	}
}

// ---- member_profiles ----

type memberRow struct {
	Wxid       string
	Name       string
	Summary    string
	Facts      []memberFact
	ArchivedAt int64
	MsgCount   int64
	UpdatedAt  int64
}

func (s *store) memberGet(wxid string) *memberRow {
	if s == nil || s.db == nil || wxid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r memberRow
	var facts string
	err := s.db.QueryRow("SELECT wxid,name,summary,facts,archived_at,msg_count,updated_at FROM member_profiles WHERE wxid=?", wxid).
		Scan(&r.Wxid, &r.Name, &r.Summary, &facts, &r.ArchivedAt, &r.MsgCount, &r.UpdatedAt)
	if err != nil {
		return nil
	}
	_ = json.Unmarshal([]byte(facts), &r.Facts)
	return &r
}

func (s *store) memberSave(r *memberRow) {
	if s == nil || s.db == nil {
		return
	}
	blob, _ := json.Marshal(r.Facts)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO member_profiles(wxid,name,summary,facts,archived_at,msg_count,updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(wxid) DO UPDATE SET name=excluded.name, summary=excluded.summary, facts=excluded.facts,
		archived_at=excluded.archived_at, msg_count=excluded.msg_count, updated_at=excluded.updated_at`,
		r.Wxid, r.Name, r.Summary, string(blob), r.ArchivedAt, r.MsgCount, r.UpdatedAt)
}

// mergeFacts 增量合并：前缀包含式去重（一方前缀是另一方前缀即视为同义），上限20条（保最新）
func mergeFacts(old, add []memberFact, now int64) []memberFact {
	prefix := func(s string) string {
		ra := []rune(s)
		if len(ra) > 12 {
			return string(ra[:12])
		}
		return s
	}
	seen := []string{}
	for _, f := range old {
		seen = append(seen, prefix(f.Content))
	}
	dup := func(p string) bool {
		for _, s := range seen {
			if len(s) > 0 && len(p) > 0 && (strings.HasPrefix(s, p) || strings.HasPrefix(p, s)) {
				return true
			}
		}
		return false
	}
	out := old
	for _, f := range add {
		f.At = now
		p := prefix(f.Content)
		if dup(p) {
			continue
		}
		seen = append(seen, p)
		out = append(out, f)
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	return out
}

// profileCandidates 找"新增未处理消息≥threshold"的成员（增量游标驱动）
func (s *store) profileCandidates(threshold, limit int) []struct {
	Wxid string
	Name string
	MaxID int64
	Count int64
} {
	type cand = struct {
		Wxid string
		Name string
		MaxID int64
		Count int64
	}
	out := []cand{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT a.wxid, COALESCE((SELECT name FROM msg_archive WHERE wxid=a.wxid ORDER BY id DESC LIMIT 1),'') AS name,
		       MAX(a.id) mx, COUNT(*) c
		FROM msg_archive a
		LEFT JOIN member_profiles p ON p.wxid = a.wxid
		WHERE a.id > COALESCE(p.archived_at, 0)
		GROUP BY a.wxid
		HAVING c >= ?
		ORDER BY c DESC LIMIT ?`, threshold, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c cand
		if rows.Scan(&c.Wxid, &c.Name, &c.MaxID, &c.Count) == nil {
			out = append(out, c)
		}
	}
	return out
}

// memberCorpus 取某成员 id>after 且 id<=before 的最近 limit 条归档（时间正序）
func (s *store) memberCorpus(wxid string, after, before int64, limit int) []string {
	out := []string{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT ts,name,kind,content FROM msg_archive WHERE wxid=? AND id>? AND id<=? ORDER BY id DESC LIMIT ?`,
		wxid, after, before, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var ts int64
		var name, kind, content string
		if rows.Scan(&ts, &name, &kind, &content) == nil {
			tag := ""
			if kind == "emoji" {
				tag = "[表情]"
			} else if kind == "image" {
				tag = "[图片]"
			}
			lines = append(lines, fmt.Sprintf("[%s] %s: %s%s", time.Unix(ts, 0).Format("01-02 15:04"), name, tag, content))
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		out = append(out, lines[i])
	}
	return out
}

// remember 对话记住：只允许记"当前发言人"
func (s *store) memberRemember(wxid, name, kind, content, chat string) bool {
	if s == nil || s.db == nil || wxid == "" || strings.TrimSpace(content) == "" {
		return false
	}
	if !factKinds[kind] {
		kind = "身份"
	}
	r := s.memberGet(wxid)
	if r == nil {
		r = &memberRow{Wxid: wxid, Name: name}
	}
	r.Name = name
	now := time.Now().Unix()
	r.Facts = mergeFacts(r.Facts, []memberFact{{Kind: kind, Content: previewOf(content, 60), Source: "对话记住", Chat: chat}}, now)
	r.UpdatedAt = now
	s.memberSave(r)
	return true
}

// memberSummary 注入用：summary + facts 摘要（≤150字）
func (s *store) memberSummary(wxid string) string {
	r := s.memberGet(wxid)
	if r == nil || (r.Summary == "" && len(r.Facts) == 0) {
		return ""
	}
	var b strings.Builder
	b.WriteString(r.Name + ":")
	if r.Summary != "" {
		b.WriteString(r.Summary)
	}
	for i, f := range r.Facts {
		seg := f.Kind + "=" + f.Content
		if b.Len()+len(seg) > 150 {
			break
		}
		if i > 0 || r.Summary != "" {
			b.WriteString("；")
		}
		b.WriteString(seg)
	}
	return b.String()
}

// ---- Web 查询 ----

type memberCard struct {
	Wxid    string `json:"wxid"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Msg7d   int    `json:"msg_7d"`
	Total   int    `json:"total"`
	EmojiP  int    `json:"emoji_pct"`
	HasAv   bool   `json:"has_avatar"`
}

func (s *store) memberCards() []memberCard {
	out := []memberCard{}
	if s == nil || s.db == nil {
		return out
	}
	week := time.Now().Add(-7 * 24 * time.Hour).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT p.wxid, p.name, p.summary,
		       (SELECT COUNT(*) FROM msg_archive m WHERE m.wxid=p.wxid AND m.ts>?) c7,
		       (SELECT COUNT(*) FROM msg_archive e WHERE e.wxid=p.wxid AND e.kind='emoji' AND e.ts>?) e7,
		       (SELECT COUNT(*) FROM msg_archive t WHERE t.wxid=p.wxid) total
		FROM member_profiles p ORDER BY p.updated_at DESC LIMIT 200`, week, week)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c memberCard
		var e7 int
		if rows.Scan(&c.Wxid, &c.Name, &c.Summary, &c.Msg7d, &e7, &c.Total) == nil {
			if c.Msg7d > 0 {
				c.EmojiP = e7 * 100 / c.Msg7d
			}
			c.HasAv = s.avatarPathUnlocked(c.Wxid) != ""
			out = append(out, c)
		}
	}
	return out
}

// avatarPathUnlocked 已持锁版本（memberCards 在 mu 内调用）
func (s *store) avatarPathUnlocked(wxid string) string {
	if s.dir == "" || !safeWxid(wxid) {
		return ""
	}
	for _, ext := range []string{".jpg", ".png", ".gif"} {
		p := filepath.Join(s.dir, "avatar_files", wxid+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

type memberDetail struct {
	Wxid     string       `json:"wxid"`
	Name     string       `json:"name"`
	Summary  string       `json:"summary"`
	Facts    []memberFact `json:"facts"`
	Hours    [24]int      `json:"hours"`
	Emotions []map[string]any `json:"emotions"`
	Recent   []map[string]any `json:"recent"`
	Updated  int64        `json:"updated_at"`
}

func (s *store) memberDetailGet(wxid string, emotions []string) *memberDetail {
	r := s.memberGet(wxid)
	if r == nil {
		return nil
	}
	d := &memberDetail{Wxid: r.Wxid, Name: r.Name, Summary: r.Summary, Facts: r.Facts, Updated: r.UpdatedAt}
	s.mu.Lock()
	rows, err := s.db.Query("SELECT ts,kind,content FROM msg_archive WHERE wxid=? ORDER BY id DESC LIMIT 500", wxid)
	s.mu.Unlock()
	if err == nil {
		emoCount := map[string]int{}
		total := 0
		var recent []map[string]any
		defer rows.Close()
		for rows.Next() {
			var ts int64
			var kind, content string
			if rows.Scan(&ts, &kind, &content) != nil {
				continue
			}
			d.Hours[time.Unix(ts, 0).Hour()]++
			if kind == "emoji" {
				emoCount[guessEmotion(content, emotions)]++
				total++
			}
			if len(recent) < 10 {
				recent = append(recent, map[string]any{"ts": ts, "kind": kind, "content": content})
			}
		}
		for e, c := range emoCount {
			if e != "" {
				d.Emotions = append(d.Emotions, map[string]any{"emotion": e, "count": c, "pct": c * 100 / maxInt(total, 1)})
			}
		}
		d.Recent = recent
	}
	// facts 倒序展示
	for i, j := 0, len(d.Facts)-1; i < j; i, j = i+1, j-1 {
		d.Facts[i], d.Facts[j] = d.Facts[j], d.Facts[i]
	}
	return d
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// memberSearch 按昵称模糊查画像（精确优先，其次包含匹配）
func (s *store) memberSearch(name string, emotions []string) *memberDetail {
	if s == nil || s.db == nil || name == "" {
		return nil
	}
	s.mu.Lock()
	var wxid string
	_ = s.db.QueryRow("SELECT wxid FROM member_profiles WHERE name=? ORDER BY updated_at DESC LIMIT 1", name).Scan(&wxid)
	if wxid == "" {
		_ = s.db.QueryRow("SELECT wxid FROM member_profiles WHERE name LIKE ? ORDER BY updated_at DESC LIMIT 1", "%"+name+"%").Scan(&wxid)
	}
	s.mu.Unlock()
	if wxid == "" {
		return nil
	}
	return s.memberDetailGet(wxid, emotions)
}

// ---- 头像 ----

func (s *store) avatarPath(wxid string) string {
	if s == nil || s.dir == "" || !safeWxid(wxid) {
		return ""
	}
	for _, ext := range []string{".jpg", ".png", ".gif"} {
		p := filepath.Join(s.dir, "avatar_files", wxid+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func safeWxid(wxid string) bool {
	if wxid == "" || len(wxid) > 64 {
		return false
	}
	for _, r := range wxid {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '@') {
			return false
		}
	}
	return true
}

// ============================================================
// profiler 后台画像器（复制 tagger 模式）
// ============================================================

type profiler struct {
	p         *hermesPlugin
	cfg       *Config
	stop      chan struct{}
	done      chan struct{}
	wake      chan struct{}
	fetching  sync.Mutex
}

func newProfiler(p *hermesPlugin) *profiler {
	return &profiler{p: p, cfg: &p.Config, stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1)}
}

func (f *profiler) Wake() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *profiler) Stop() {
	close(f.stop)
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
	}
}

func (f *profiler) loop() {
	defer close(f.done)
	tick := time.NewTicker(3 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-tick.C:
			f.batch()
		case <-f.wake:
			time.Sleep(5 * time.Second) // 等归档 flush
			f.batch()
		}
	}
}

func (f *profiler) batch() {
	if !f.fetching.TryLock() {
		return
	}
	defer f.fetching.Unlock()
	if !f.cfg.ProfileEnabled {
		return
	}
	cands := f.p.store.profileCandidates(f.cfg.ProfileThreshold, f.cfg.ProfileBatch)
	for _, c := range cands {
		select {
		case <-f.stop:
			return
		default:
		}
		f.extractOne(c.Wxid, c.Name, c.MaxID)
	}
}

func (f *profiler) extractOne(wxid, name string, maxID int64) {
	s := f.p.store
	corpus := s.memberCorpus(wxid, 0, maxID, 200)
	if len(corpus) < 5 {
		// 语料太少也推进游标，避免反复扫描
		r := s.memberGet(wxid)
		if r == nil {
			r = &memberRow{Wxid: wxid, Name: name}
		}
		r.ArchivedAt = maxID
		s.memberSave(r)
		return
	}
	prompt := fmt.Sprintf(`以下是微信群成员「%s」最近的聊天记录。请只根据这些记录，抽取关于这个人本身的稳定事实。

输出严格 JSON（不要其他文字）：
{"summary":"一句话人设,不超过25字","facts":[{"kind":"口头禅|喜好|身份|关系|flag|名场面","content":"事实描述(不超过20字)","quote":"依据原句(不超过30字)"}]}

规则：
- 只抽取关于此人自身的稳定信息，不抽取群聊话题本身
- 口头禅=反复出现的表达；flag=承诺或计划；名场面=金句或标志性事件；关系=与其他人的互动关系
- 丢弃并绝不记录任何手机号、住址、身份证、银行卡、转账类内容
- 没有值得记的就输出 {"summary":"","facts":[]}
- 最多8条，全部用中文

聊天记录：
%s`, name, strings.Join(corpus, "\n"))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := f.p.eng.Complete(ctx, runRequest{
		SessionKey: "wx:system:member-profile",
		Profile:    f.cfg.FastProfile,
		Prompt:     prompt,
	})
	if err != nil {
		slog.Warn("[ghp] 画像抽取失败", "wxid", wxid, "err", err)
		return
	}
	s.addUsage(res.Usage, f.cfg.FastProfile)

	var out struct {
		Summary string       `json:"summary"`
		Facts   []memberFact `json:"facts"`
	}
	txt := strings.TrimSpace(res.Text)
	txt = strings.TrimPrefix(txt, "```json")
	txt = strings.TrimPrefix(txt, "```")
	txt = strings.TrimSuffix(txt, "```")
	if err := json.Unmarshal([]byte(strings.TrimSpace(txt)), &out); err != nil {
		slog.Warn("[ghp] 画像抽取解析失败", "wxid", wxid, "err", err, "raw", previewOf(txt, 200))
		return
	}
	valid := out.Facts[:0]
	for _, fact := range out.Facts {
		if factKinds[fact.Kind] && strings.TrimSpace(fact.Content) != "" {
			valid = append(valid, memberFact{
				Kind: fact.Kind, Content: fact.Content, Source: "后台抽取",
				Quote: previewOf(fact.Quote, 40),
			})
		}
	}

	r := s.memberGet(wxid)
	if r == nil {
		r = &memberRow{Wxid: wxid, Name: name}
	}
	r.Name = name
	now := time.Now().Unix()
	if out.Summary != "" {
		r.Summary = out.Summary
	}
	r.Facts = mergeFacts(r.Facts, valid, now)
	r.ArchivedAt = maxID
	r.UpdatedAt = now
	s.memberSave(r)
	if len(valid) > 0 || out.Summary != "" {
		slog.Info("[ghp] 画像更新", "wxid", wxid, "name", name, "facts", len(valid), "summary", out.Summary)
	}

	// 头像补拉
	go f.p.fetchAvatar(wxid)
}

// fetchAvatar 从群成员缓存下载头像
func (p *hermesPlugin) fetchAvatar(wxid string) {
	if p.store.avatarPath(wxid) != "" || !p.Config.ProfileEnabled {
		return
	}
	url := ""
	for key, as := range p.disp.activeCopy() {
		if !as.IsGroup {
			continue
		}
		if m := p.chatroom.GetMember(as.GroupID, wxid); m != nil && m.GetAvatar() != "" {
			url = m.GetAvatar()
			_ = key
			break
		}
	}
	if url == "" {
		return
	}
	data, err := downloadFile(url, 15*time.Second)
	if err != nil || len(data) < 100 || len(data) > 500*1024 {
		return
	}
	ext := sniffImage(data)
	if ext == "" {
		return
	}
	dir := filepath.Join(p.store.dir, "avatar_files")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, wxid+"."+ext), data, 0o644)
}
