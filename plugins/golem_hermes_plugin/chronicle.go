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
// 群史官：每日定时出刊（四文风轮换）+ 号外 + 存档回看
// 出刊开关挂群级行内配置（chat_settings.chronicle）
// ============================================================

// chronicleStyle 文风定义（config.toml 持久化，Web 文风坊可编辑）
type chronicleStyle struct {
	Name    string `toml:"name" json:"name"`
	Hint    string `toml:"hint" json:"hint"`
	Enabled bool   `toml:"enabled" json:"enabled"`
}

const defaultChroniclePrompt = `你是微信群「%s」的史官，为本群撰写今日第 %d 期日报（今日真实聊天记录见文末素材）。

【文风要求】%s

【栏目结构】（markdown 全文，顺序固定）
# 《%s日报》第 %d 期
> %s · 天佑本群 · 风调雨顺

## 本日纪要
（3-5句概括今日大事，文风要求全程贯彻）

## 金句榜
（今日最佳语录 top3，带出处发言人）

## 梗之诞生
（今日新梗考古：首现→传播→定性；没有新梗就写"今日无新梗，旧梗温习"并挑一个旧梗温习）

## 名场面
（还原一段真实对话的微型剧场，2-4条消息）

## 祥瑞灾异
（数据天象：表情出鞘统计、活跃时段异常、复读事件等，用占星口吻写）

## 史官曰
（一段点评，毒舌或深情二选一，不超过三句）

【硬性规则】
- 只依据素材真实内容，绝不编造未发生的对话；素材不足的栏目如实说明
- 绝不出现手机号、住址、证件号等隐私
- 总长控制在 500 字内，金句必须带发言人
- 输出严格 JSON：{"headline":"本期大标题(<=20字)","markdown":"报纸全文"}`

type chronicleEntry struct {
	ID       int64  `json:"id"`
	Chat     string `json:"chat"`
	ChatName string `json:"chat_name"`
	Date     string `json:"date"`
	Issue    int    `json:"issue"`
	Kind     string `json:"kind"` // 正刊 | 号外
	Style    string `json:"style"`
	Headline string `json:"headline"`
	MD       string `json:"md,omitempty"`
	ImgFile  string `json:"-"`
	CreatedAt int64 `json:"created_at"`
}

type chronicle struct {
	p    *hermesPlugin
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	published map[string]string // chat -> 最后出刊日期（防重复）
	extraDone map[string]string // chat -> 号外已发日期
	running  bool
}

func newChronicle(p *hermesPlugin) *chronicle {
	return &chronicle{p: p, stop: make(chan struct{}), done: make(chan struct{}),
		published: map[string]string{}, extraDone: map[string]string{}}
}

func (c *chronicle) loop() {
	defer close(c.done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
			c.tickFire()
		}
	}
}

func (c *chronicle) Stop() {
	close(c.stop)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
}

// tickFire 定时到点出刊 + 号外检查
func (c *chronicle) tickFire() {
	cfg := &c.p.Config
	if !cfg.ChronicleEnabled {
		return
	}
	now := time.Now()
	atTime := now.Hour() == cfg.ChronicleHour && now.Minute() == cfg.ChronicleMinute
	today := now.Format("2006-01-02")
	for _, chat := range c.chronicleChats() {
		if atTime {
			c.mu.Lock()
			last := c.published[chat]
			c.mu.Unlock()
			// 去重落库：内存守卫在 /pm reload 后清零，曾导致正刊/号外重复出刊
			if last != today && !c.p.store.chroniclePublished(chat, today, "正刊") {
				c.publishOne(chat, today, "正刊")
			}
		}
		// 号外：当日消息量超阈值且未发过
		if cfg.ChronicleExtraThreshold > 0 {
			c.mu.Lock()
			extra := c.extraDone[chat]
			c.mu.Unlock()
			if extra != today && !c.p.store.chroniclePublished(chat, today, "号外") &&
				c.p.store.archiveCountSince(chat, today) >= cfg.ChronicleExtraThreshold {
				c.publishOne(chat, today, "号外")
				c.mu.Lock()
				c.extraDone[chat] = today
				c.mu.Unlock()
			}
		}
	}
}

// chronicleChats 启用史官的群（群级行内配置）
func (c *chronicle) chronicleChats() []string {
	out := []string{}
	for _, ca := range c.p.metrics.chatsSnapshot() {
		if !ca.IsGroup {
			continue
		}
		if c.p.effectiveChatConfig(ca.Receiver).Chronicle {
			out = append(out, ca.Receiver)
		}
	}
	return out
}

// PublishNow Web「立即出刊」
func (c *chronicle) PublishNow(chat string) (int64, error) {
	if chat != "" {
		return c.publishOne(chat, time.Now().Format("2006-01-02"), "正刊")
	}
	var lastID int64
	for _, ch := range c.chronicleChats() {
		id, err := c.publishOne(ch, time.Now().Format("2006-01-02"), "正刊")
		if err == nil {
			lastID = id
		}
	}
	return lastID, nil
}

// publishOne 单群出刊全流程：取材→选文风→写作→渲染→发布→存档
func (c *chronicle) publishOne(chat, date, kind string) (int64, error) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return 0, fmt.Errorf("出刊进行中")
	}
	c.running = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()

	p := c.p
	if p.eng == nil {
		return 0, fmt.Errorf("引擎未就绪")
	}
	chatName := chat
	if cs := p.metrics.chatsSnapshot(); len(cs) > 0 {
		for _, ca := range cs {
			if ca.Receiver == chat && ca.Name != "" {
				chatName = ca.Name
			}
		}
	}
	issue := p.store.chronicleNextIssue(chat, date, kind)
	style := c.pickStyle(chat)
	material := c.buildMaterial(chat, chatName, date)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res, err := p.eng.Complete(ctx, runRequest{
		SessionKey: "wx:system:chronicle",
		Profile:    p.Config.FullProfile,
		Prompt: fmt.Sprintf(defaultChroniclePrompt, chatName, issue, style.Hint, chatName, issue, date) + "\n\n【素材】\n" + material,
	})
	if err != nil {
		slog.Warn("[ghp] 史官写作失败", "chat", chatName, "err", err)
		return 0, err
	}
	p.store.addUsage(res.Usage, p.Config.FullProfile)

	var out struct {
		Headline string `json:"headline"`
		Markdown string `json:"markdown"`
	}
	txt := strings.TrimSpace(res.Text)
	txt = strings.TrimPrefix(txt, "```json")
	txt = strings.TrimPrefix(txt, "```")
	txt = strings.TrimSuffix(txt, "```")
	txt = strings.TrimSpace(txt)
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		// 模型常在 JSON 字符串里输出字面换行（非法 JSON），抢救性提取
		headline, md := rescueChronicleJSON(txt)
		if md == "" {
			slog.Warn("[ghp] 史官输出解析失败", "chat", chatName, "raw", previewOf(txt, 160))
			return 0, fmt.Errorf("输出解析失败")
		}
		out.Headline, out.Markdown = headline, md
	}
	if out.Markdown == "" {
		return 0, fmt.Errorf("输出为空")
	}
	if out.Headline == "" {
		out.Headline = previewOf(out.Markdown, 20)
	}

	// 渲染报纸卡片（gg 插件；caller 未注入或失败时降级纯文本）
	var img []byte
	if p.caller != nil {
		_, data, err := callWithTimeout(p.caller, "markdown.to.image", map[string]string{"context": out.Markdown}, 120*time.Second)
		if err != nil || len(data) == 0 {
			slog.Warn("[ghp] 报纸渲染失败，降级纯文本", "err", err)
		} else {
			img = data
		}
	}

	// 存档
	entry := p.store.chronicleSave(chat, chatName, date, issue, kind, style.Name, out.Headline, out.Markdown, img, c.imgDir())
	// 发布（out 未就绪时仅存档不出站）
	if p.out != nil {
		if img != nil {
			if serr := p.out.sendImage(chat, img); serr != nil {
				slog.Warn("[ghp] 报纸发送失败，降级文本", "err", serr)
				_ = p.out.sendText(chat, out.Markdown)
			}
		} else {
			_ = p.out.sendText(chat, out.Markdown)
		}
	}
	c.mu.Lock()
	c.published[chat] = date
	c.mu.Unlock()
	slog.Info("[ghp] 史官出刊", "chat", chatName, "issue", issue, "kind", kind, "style", style.Name, "headline", out.Headline)
	p.metrics.addLog("INFO", fmt.Sprintf("[史官] 出刊 %s 第%d期(%s) · %s", chatName, issue, kind, out.Headline))
	return entry.ID, nil
}

func (c *chronicle) imgDir() string {
	return filepath.Join(c.p.dataDir, "chronicle_files")
}

// pickStyle 选文风：启用池中"该群最久未用"优先
func (c *chronicle) pickStyle(chat string) chronicleStyle {
	cfg := &c.p.Config
	pool := []chronicleStyle{}
	for _, st := range cfg.ChronicleStyles {
		if st.Enabled {
			pool = append(pool, st)
		}
	}
	if len(pool) == 0 {
		return chronicleStyle{Name: "太史公体", Hint: "以《史记》笔法行文，篇末『太史公曰』点评", Enabled: true}
	}
	if cfg.ChronicleStyleMode == "fixed" {
		for _, st := range pool {
			if st.Name == cfg.ChronicleFixedStyle {
				return st
			}
		}
	}
	last := c.p.store.chronicleLastStyle(chat)
	for _, st := range pool {
		if st.Name != last {
			return st
		}
	}
	return pool[0]
}

// buildMaterial 取材：当日消息 + 画像 + 表情统计 + 昨日标题
func (c *chronicle) buildMaterial(chat, chatName, date string) string {
	var b strings.Builder
	msgs := c.p.store.archiveOfDay(chat, date, 150)
	if len(msgs) == 0 {
		return "(今日暂无语料)"
	}
	b.WriteString("【今日聊天记录(节选)】\n")
	for _, m := range msgs {
		b.WriteString(m + "\n")
	}
	b.WriteString("\n【成员画像(参考)】\n")
	for _, pf := range c.p.store.memberSummariesBrief(chat, date, 6) {
		b.WriteString(pf + "\n")
	}
	if top := c.p.store.emojiStatOfDay(chat, date); top != "" {
		b.WriteString("\n【表情天象】" + top + "\n")
	}
	if prev := c.p.store.chroniclePrevHeadline(chat, date); prev != "" {
		b.WriteString("\n【昨日头版(避免重复玩梗)】" + prev + "\n")
	}
	return b.String()
}

// ---- store 层：chronicles 表 ----

func (s *store) initChronicle() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS chronicles(
		id INTEGER PRIMARY KEY AUTOINCREMENT, chat TEXT, chat_name TEXT, date TEXT,
		issue INTEGER, kind TEXT, style TEXT, headline TEXT, md TEXT, img_file TEXT, created_at INTEGER)`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_chron_chat ON chronicles(chat, date)`)
}

// chroniclePublished 当日该群该类型是否已出刊过（以 chronicles 表真实记录为准，
// 内存守卫 reload 后清零时兜底——号外风暴事故的根治）
func (s *store) chroniclePublished(chat, date, kind string) bool {
	if s == nil || s.db == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM chronicles WHERE chat=? AND date=? AND kind=?", chat, date, kind).Scan(&n)
	return n > 0
}

// chronicleNextIssue 期号 = 该群正刊最大期 + 1
func (s *store) chronicleNextIssue(chat, date, kind string) int {
	if s == nil || s.db == nil {
		return 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var mx int
	_ = s.db.QueryRow("SELECT COALESCE(MAX(issue),0) FROM chronicles WHERE chat=? AND kind='正刊'", chat).Scan(&mx)
	_ = date
	_ = kind
	return mx + 1
}

func (s *store) chronicleLastStyle(chat string) string {
	if s == nil || s.db == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var st string
	_ = s.db.QueryRow("SELECT style FROM chronicles WHERE chat=? ORDER BY id DESC LIMIT 1", chat).Scan(&st)
	return st
}

func (s *store) chroniclePrevHeadline(chat, date string) string {
	if s == nil || s.db == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var h string
	_ = s.db.QueryRow("SELECT headline FROM chronicles WHERE chat=? AND date<? ORDER BY id DESC LIMIT 1", chat, date).Scan(&h)
	return h
}

func (s *store) chronicleSave(chat, chatName, date string, issue int, kind, style, headline, md string, img []byte, imgDir string) chronicleEntry {
	e := chronicleEntry{Chat: chat, ChatName: chatName, Date: date, Issue: issue, Kind: kind, Style: style, Headline: headline, CreatedAt: time.Now().Unix()}
	if s == nil || s.db == nil {
		return e
	}
	imgFile := ""
	if len(img) > 0 {
		_ = os.MkdirAll(imgDir, 0o755)
		basename := fmt.Sprintf("%s_%s_%d.png", safeFileBase(chat), date, issue)
		imgFile = filepath.Join(imgDir, basename)
		if err := os.WriteFile(imgFile, img, 0o644); err != nil {
			imgFile = ""
		}
	}
	e.ImgFile = imgFile
	s.mu.Lock()
	r, err := s.db.Exec("INSERT INTO chronicles(chat,chat_name,date,issue,kind,style,headline,md,img_file,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		chat, chatName, date, issue, kind, style, headline, md, imgFile, e.CreatedAt)
	s.mu.Unlock()
	if err == nil {
		id, _ := r.LastInsertId()
		e.ID = id
	}
	return e
}

func (s *store) chronicleList(chat string, limit int) []chronicleEntry {
	out := []chronicleEntry{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := "SELECT id,chat,chat_name,date,issue,kind,style,headline,img_file,created_at FROM chronicles"
	var args []any
	if chat != "" {
		q += " WHERE chat=?"
		args = append(args, chat)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e chronicleEntry
		if rows.Scan(&e.ID, &e.Chat, &e.ChatName, &e.Date, &e.Issue, &e.Kind, &e.Style, &e.Headline, &e.ImgFile, &e.CreatedAt) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (s *store) chronicleGet(id int64) *chronicleEntry {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var e chronicleEntry
	var md string
	err := s.db.QueryRow("SELECT id,chat,chat_name,date,issue,kind,style,headline,md,img_file,created_at FROM chronicles WHERE id=?", id).
		Scan(&e.ID, &e.Chat, &e.ChatName, &e.Date, &e.Issue, &e.Kind, &e.Style, &e.Headline, &md, &e.ImgFile, &e.CreatedAt)
	if err != nil {
		return nil
	}
	e.MD = md
	return &e
}

// ---- 取材查询 ----

func (s *store) archiveCountSince(chat, date string) int {
	if s == nil || s.db == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	ts, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	_ = s.db.QueryRow("SELECT COUNT(*) FROM msg_archive WHERE chat=? AND ts>=?", chat, ts.Unix()).Scan(&n)
	return n
}

func (s *store) archiveOfDay(chat, date string, limit int) []string {
	out := []string{}
	if s == nil || s.db == nil {
		return out
	}
	ts, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT ts,name,kind,content FROM msg_archive WHERE chat=? AND ts>=? AND ts<? ORDER BY id DESC LIMIT ?",
		chat, ts.Unix(), ts.Add(24*time.Hour).Unix(), limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var t int64
		var name, kind, content string
		if rows.Scan(&t, &name, &kind, &content) == nil {
			tag := ""
			if kind == "emoji" {
				tag = "[表情]"
			} else if kind == "image" {
				tag = "[图片]"
			}
			lines = append(lines, fmt.Sprintf("[%s] %s: %s%s", time.Unix(t, 0).Format("15:04"), name, tag, content))
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		out = append(out, lines[i])
	}
	return out
}

func (s *store) memberSummariesBrief(chat, date string, limit int) []string {
	out := []string{}
	if s == nil || s.db == nil {
		return out
	}
	ts, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	s.mu.Lock()
	rows, err := s.db.Query(`SELECT a.wxid, MAX(a.name), COUNT(*) FROM msg_archive a WHERE a.chat=? AND a.ts>=? GROUP BY a.wxid ORDER BY 3 DESC LIMIT ?`,
		chat, ts.Unix(), limit)
	s.mu.Unlock()
	if err != nil {
		return out
	}
	defer rows.Close()
	var ids [][2]string
	for rows.Next() {
		var wxid, name string
		var n int
		if rows.Scan(&wxid, &name, &n) == nil {
			ids = append(ids, [2]string{wxid, name})
		}
	}
	rows.Close()
	for _, id := range ids {
		if sm := s.memberSummary(id[0]); sm != "" {
			out = append(out, sm+"（今日发言）")
		}
	}
	return out
}

func (s *store) emojiStatOfDay(chat, date string) string {
	if s == nil || s.db == nil {
		return ""
	}
	ts, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM msg_archive WHERE chat=? AND kind='emoji' AND ts>=? AND ts<?", chat, ts.Unix(), ts.Add(24*time.Hour).Unix()).Scan(&n)
	if n == 0 {
		return "今日表情太平，出鞘零次，天下无事。"
	}
	return fmt.Sprintf("今日表情出鞘 %d 次，军备兴旺。", n)
}

func safeFileBase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "chat"
	}
	return b.String()
}

// rescueChronicleJSON 抢救性解析：模型常在 JSON 字符串内输出字面换行导致非法 JSON，
// 这里按字段边界手工截取并反转义
func rescueChronicleJSON(txt string) (headline, markdown string) {
	trim := strings.TrimSpace(txt)
	// 截取首个 { 到最后一个 }
	if i := strings.Index(trim, "{"); i >= 0 {
		trim = trim[i:]
	}
	if j := strings.LastIndex(trim, "}"); j >= 0 {
		trim = trim[:j+1]
	}
	grab := func(key string) (string, bool) {
		k := "\"" + key + "\""
		ki := strings.Index(trim, k)
		if ki < 0 {
			return "", false
		}
		rest := trim[ki+len(k):]
		ci := strings.Index(rest, ":")
		if ci < 0 {
			return "", false
		}
		rest = strings.TrimLeft(rest[ci+1:], " \t")
		if !strings.HasPrefix(rest, "\"") {
			return "", false
		}
		rest = rest[1:]
		// 找未转义的收尾引号：优先行尾 ",\n 或 "\n 或 "}$ 或 "}
		end := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == '"' {
				after := rest[i+1:]
				if after == "" || after[0] == '}' || after[0] == ',' || after[0] == '\n' || after[0] == '\r' || after[0] == ' ' {
					end = i
					break
				}
			}
		}
		if end < 0 {
			return "", false
		}
		raw := rest[:end]
		// 反转义（容错：先 \n 再 \\，字面换行原样保留）
		raw = strings.ReplaceAll(raw, "\\n", "\n")
		raw = strings.ReplaceAll(raw, "\\t", "\t")
		raw = strings.ReplaceAll(raw, "\\\"", "\"")
		raw = strings.ReplaceAll(raw, "\\\\", "\\")
		return raw, true
	}
	if h, ok := grab("headline"); ok {
		headline = h
	}
	if m, ok := grab("markdown"); ok {
		markdown = m
	}
	return headline, markdown
}
