package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sbgayhub/golem/sdk/message"
	_ "modernc.org/sqlite"
)

// store 插件本地存储：表情库 + kv（用量记账）+ 消息归档 + 成员画像，单 SQLite 文件
type store struct {
	db  *sql.DB
	dir string
	mu  sync.Mutex

	archCh   chan archiveMsg
	archDone chan struct{}
}

func newStore(path, stickerDir string) *store {
	s := &store{dir: stickerDir}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		slog.Error("[ghp] 打开数据库失败", "path", path, "err", err)
		return s
	}
	s.db = db
	_, _ = db.Exec("PRAGMA journal_mode=WAL")
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS stickers(
		md5 TEXT PRIMARY KEY, file TEXT, desc TEXT DEFAULT '', emotion TEXT DEFAULT '', keywords TEXT DEFAULT '',
		attempts INTEGER DEFAULT 0, use_count INTEGER DEFAULT 0, last_used INTEGER DEFAULT 0,
		created_at INTEGER)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS msg_archive(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER, chat TEXT, wxid TEXT, name TEXT, kind TEXT, content TEXT)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_archive_wxid ON msg_archive(wxid, id)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS member_profiles(
		wxid TEXT PRIMARY KEY, name TEXT, summary TEXT DEFAULT '', facts TEXT DEFAULT '[]',
		archived_at INTEGER DEFAULT 0, msg_count INTEGER DEFAULT 0, updated_at INTEGER)`)
	s.initSettings()
	s.initChronicle()
	s.initImagegen()
	s.initPersonas()
	s.initVideoRing()
	s.migrateArchiveImageLabels()
	s.startArchive()
	return s
}

// migrateArchiveImageLabels 一次性清洗：宿主解析零值年代归档的图片行带着
// 误导性的 "[0 x 0] 0B"（模型照念导致 0B 梗，画像/史官语料也被污染）。
// 幂等：清洗过的行不再匹配 LIKE，重复执行无副作用。
func (s *store) migrateArchiveImageLabels() {
	if s == nil || s.db == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE msg_archive SET content='[图片]'
		WHERE kind='image' AND content LIKE '%[0 x 0] 0B%'`)
	if err != nil {
		slog.Warn("[ghp] 归档图片标签清洗失败", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Info(fmt.Sprintf("[ghp] 归档清洗：%d 行误导性 0B 图片标签已修正", n))
	}
}

// ---- kv 通用读写（画坊运行时配置、配额计数等） ----

func (s *store) kvGet(k string) string {
	if s == nil || s.db == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v string
	_ = s.db.QueryRow("SELECT v FROM kv WHERE k=?", k).Scan(&v)
	return v
}

func (s *store) kvSet(k, v string) {
	if s == nil || s.db == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec("INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, v)
}

// lastArchiveTs 某群归档里最后一条消息的时刻（冷场看护 reload 后播种真人时钟用；0=无归档）
func (s *store) lastArchiveTs(chat string) int64 {
	if s == nil || s.db == nil || chat == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ts int64
	_ = s.db.QueryRow("SELECT COALESCE(MAX(ts),0) FROM msg_archive WHERE chat=?", chat).Scan(&ts)
	return ts
}

func (s *store) close() {
	s.stopArchive()
	if s.db != nil {
		_ = s.db.Close()
	}
}

// ---- 表情收集 ----

// collectSticker 下载入库；返回是否为新收集（用于唤醒打标器）
func (s *store) collectSticker(e *message.EmojiData) bool {
	if s == nil || s.db == nil || e == nil || e.GetMedia() == nil {
		return false
	}
	md5v := strings.ToLower(e.GetMedia().GetMd5())
	url := e.GetMedia().GetUrl()
	if md5v == "" || url == "" {
		return false
	}
	s.mu.Lock()
	var one string
	err := s.db.QueryRow("SELECT md5 FROM stickers WHERE md5=?", md5v).Scan(&one)
	s.mu.Unlock()
	if err == nil {
		return false // 已入库
	}
	data, err := downloadFile(url, 20*time.Second)
	if err != nil || len(data) < 1024 || len(data) > maxEmojiBytes {
		return false
	}
	ext := sniffImage(data)
	if ext == "" {
		return false // 加密或非图片数据
	}
	path := filepath.Join(s.dir, md5v+"."+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return false
	}
	s.mu.Lock()
	_, _ = s.db.Exec("INSERT OR IGNORE INTO stickers(md5,file,desc,created_at) VALUES(?,?,?,?)", md5v, path, e.GetDesc(), time.Now().Unix())
	s.mu.Unlock()
	slog.Info("[ghp] 收集表情", "md5", md5v, "size", len(data), "desc", e.GetDesc())
	return true
}

func sniffImage(data []byte) string {
	switch {
	case len(data) > 3 && data[0] == 'G' && data[1] == 'I' && data[2] == 'F':
		return "gif"
	case len(data) > 2 && data[0] == 0xFF && data[1] == 0xD8:
		return "jpg"
	case len(data) > 3 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G':
		return "png"
	default:
		return ""
	}
}

func downloadFile(url string, timeout time.Duration) ([]byte, error) {
	hc := &http.Client{Timeout: timeout}
	resp, err := hc.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxEmojiBytes+1024))
}

// pickSticker 按情绪/关键词选取：use_count 少者优先 + 冷却
func (s *store) pickSticker(emotion, keyword string, cooldown time.Duration) []byte {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file := s.querySticker(emotion, keyword, cooldown)
	if file == "" && cooldown > 0 {
		file = s.querySticker(emotion, keyword, 0) // 冷却放宽兜底
	}
	if file == "" {
		_ = s.db.QueryRow("SELECT file FROM stickers ORDER BY RANDOM() LIMIT 1").Scan(&file)
	}
	if file == "" {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	_, _ = s.db.Exec("UPDATE stickers SET use_count=use_count+1, last_used=? WHERE file=?", time.Now().Unix(), file)
	return data
}

func (s *store) querySticker(emotion, keyword string, cooldown time.Duration) string {
	q := "SELECT file FROM stickers WHERE 1=1"
	var args []any
	if emotion != "" {
		q += " AND emotion=?"
		args = append(args, emotion)
	}
	if keyword != "" {
		q += " AND (keywords LIKE ? OR emotion=?)"
		args = append(args, "%"+keyword+"%", keyword)
	}
	if cooldown > 0 {
		q += " AND (last_used=0 OR last_used < ?)"
		args = append(args, time.Now().Add(-cooldown).Unix())
	}
	q += " ORDER BY use_count ASC, RANDOM() LIMIT 1"
	var file string
	_ = s.db.QueryRow(q, args...).Scan(&file)
	return file
}

func (s *store) stickerCounts() (total, tagged int) {
	if s == nil || s.db == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.db.QueryRow("SELECT COUNT(*) FROM stickers").Scan(&total)
	_ = s.db.QueryRow("SELECT COUNT(*) FROM stickers WHERE emotion != ''").Scan(&tagged)
	return total, tagged
}

// ---- 用量记账 ----

type usageDetail struct {
	P         int               `json:"p"`
	C         int               `json:"c"`
	ByProfile map[string][2]int `json:"by_profile,omitempty"` // profile -> [prompt, completion]
}

func (s *store) addUsage(u usage, profile string) {
	if s == nil || s.db == nil || u.total() == 0 {
		return
	}
	day := time.Now().Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur usageDetail
	var raw string
	if err := s.db.QueryRow("SELECT v FROM kv WHERE k=?", "usage:"+day).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &cur)
	}
	cur.P += u.PromptTokens
	cur.C += u.CompletionTokens
	if profile != "" {
		if cur.ByProfile == nil {
			cur.ByProfile = map[string][2]int{}
		}
		e := cur.ByProfile[profile]
		e[0] += u.PromptTokens
		e[1] += u.CompletionTokens
		cur.ByProfile[profile] = e
	}
	blob, _ := json.Marshal(cur)
	_, _ = s.db.Exec("INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", "usage:"+day, string(blob))
}

func (s *store) usageTodayDetail() usageDetail {
	var cur usageDetail
	if s == nil || s.db == nil {
		return cur
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	var raw string
	if err := s.db.QueryRow("SELECT v FROM kv WHERE k=?", "usage:"+day).Scan(&raw); err != nil {
		return cur
	}
	_ = json.Unmarshal([]byte(raw), &cur)
	return cur
}

func (s *store) usageToday() (prompt, completion int) {
	d := s.usageTodayDetail()
	return d.P, d.C
}

// usageDays 最近 n 天趋势（按天升序）
func (s *store) usageDays(n int) []map[string]any {
	out := []map[string]any{}
	if s == nil || s.db == nil || n <= 0 {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT k, v FROM kv WHERE k LIKE 'usage:%' ORDER BY k DESC LIMIT ?", n)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, raw string
		if rows.Scan(&k, &raw) != nil {
			continue
		}
		var cur usageDetail
		_ = json.Unmarshal([]byte(raw), &cur)
		out = append(out, map[string]any{
			"date": strings.TrimPrefix(k, "usage:"),
			"prompt": cur.P, "completion": cur.C, "by_profile": cur.ByProfile,
		})
	}
	// 反转为升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---- 监控台查询 ----

func (s *store) stickerByEmotion() []map[string]any {
	out := []map[string]any{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT COALESCE(NULLIF(emotion,''),'(未打标)') em, COUNT(*) FROM stickers GROUP BY em ORDER BY 2 DESC")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var em string
		var n int
		if rows.Scan(&em, &n) == nil {
			out = append(out, map[string]any{"emotion": em, "count": n})
		}
	}
	return out
}

func (s *store) stickerRecent(emotion string, limit int) []map[string]any {
	out := []map[string]any{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := "SELECT md5, COALESCE(NULLIF(emotion,''),''), keywords, use_count, last_used, created_at FROM stickers"
	var args []any
	if emotion != "" {
		q += " WHERE emotion=?"
		args = append(args, emotion)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var md5, em, kw string
		var uc, lu, ca int64
		if rows.Scan(&md5, &em, &kw, &uc, &lu, &ca) == nil {
			out = append(out, map[string]any{
				"md5": md5, "emotion": em, "keywords": kw, "use_count": uc,
				"last_used": lu, "created_at": ca,
				"file": "/api/stickers/file/" + md5,
			})
		}
	}
	return out
}

func (s *store) stickerQueue() []string {
	out := []string{}
	if s == nil || s.db == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT md5 FROM stickers WHERE emotion='' AND attempts<3 ORDER BY created_at LIMIT 20")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var md5 string
		if rows.Scan(&md5) == nil {
			out = append(out, md5)
		}
	}
	return out
}

func (s *store) stickerFile(md5 string) []byte {
	if s == nil || s.db == nil || md5 == "" || strings.ContainsAny(md5, "/\\.%") {
		return nil
	}
	s.mu.Lock()
	var file string
	_ = s.db.QueryRow("SELECT file FROM stickers WHERE md5=?", md5).Scan(&file)
	s.mu.Unlock()
	if file == "" {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	return data
}

func (s *store) tokenExceeded(capTokens int) bool {
	if capTokens <= 0 {
		return false
	}
	p, c := s.usageToday()
	return p+c > capTokens
}

// ---- 视觉打标 ----

type tagger struct {
	store *store
	eng   Engine
	cfg   *Config
	stop  chan struct{}
	done  chan struct{}
	wake  chan struct{} // 收集到新表情时立即唤醒扫描
}

func newTagger(s *store, e Engine, cfg *Config) *tagger {
	return &tagger{store: s, eng: e, cfg: cfg, stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1)}
}

// Wake 非阻塞唤醒一次打标扫描
func (t *tagger) Wake() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *tagger) loop() {
	defer close(t.done)
	tick := time.NewTicker(3 * time.Minute)
	defer tick.Stop()
	t.tagBatch()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.tagBatch()
		case <-t.wake:
			time.Sleep(2 * time.Second) // 等下载落盘稳定
			t.tagBatch()
		}
	}
}

func (t *tagger) Stop() {
	close(t.stop)
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
}

func (t *tagger) tagBatch() {
	if t == nil || t.store == nil || t.store.db == nil || t.eng == nil {
		return
	}
	type row struct {
		md5, file, desc string
	}
	t.store.mu.Lock()
	rows := []row{}
	rs, err := t.store.db.Query("SELECT md5,file,desc FROM stickers WHERE emotion='' AND attempts<3 ORDER BY created_at LIMIT 5")
	if err == nil {
		for rs.Next() {
			var r row
			if rs.Scan(&r.md5, &r.file, &r.desc) == nil {
				rows = append(rows, r)
			}
		}
		rs.Close()
	}
	t.store.mu.Unlock()

	for _, r := range rows {
		select {
		case <-t.stop:
			return
		default:
		}
		var emotion string
		var keywords []string
		if r.desc != "" {
			// 优先按微信表情名做文本分类（无需视觉模型）
			emotion, keywords = t.classifyText(r.desc)
		} else {
			data, err := os.ReadFile(r.file)
			if err != nil {
				t.mark(r.md5, "", nil, 99)
				continue
			}
			mime := "image/gif"
			if ext := sniffImage(data); ext == "jpg" {
				mime = "image/jpeg"
			} else if ext == "png" {
				mime = "image/png"
			}
			emotion, keywords = t.classifyVision(data, mime)
		}
		t.mark(r.md5, emotion, keywords, 1)
		if emotion != "" {
			slog.Info("[ghp] 表情打标完成", "md5", r.md5, "emotion", emotion)
		}
	}
}

func (t *tagger) tagProfile() string {
	if t.cfg.VisionProfile != "" {
		return t.cfg.VisionProfile
	}
	return t.cfg.FastProfile
}

func (t *tagger) classifyText(desc string) (string, []string) {
	prompt := fmt.Sprintf(
		"微信表情包的名字是「%s」。请把它分类到以下情绪之一: [%s]，并给出最多5个中文关键词。只输出 JSON: {\"emotion\":\"...\",\"keywords\":[\"...\"]}。",
		desc, strings.Join(t.cfg.StickerEmotions, ", "))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := t.eng.Complete(ctx, runRequest{
		SessionKey: "wx:system:sticker-tag",
		Profile:    t.tagProfile(),
		Prompt:     prompt,
	})
	if err != nil {
		return "", nil
	}
	t.store.addUsage(res.Usage, t.tagProfile())
	return parseEmotionJSON(res.Text, t.cfg.StickerEmotions)
}

func (t *tagger) mark(md5, emotion string, keywords []string, attempts int) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if emotion == "" {
		_, _ = t.store.db.Exec("UPDATE stickers SET attempts=attempts+? WHERE md5=?", attempts, md5)
		return
	}
	_, _ = t.store.db.Exec("UPDATE stickers SET emotion=?, keywords=?, attempts=attempts+? WHERE md5=?",
		emotion, strings.Join(keywords, ","), attempts, md5)
}

func (t *tagger) classifyVision(img []byte, mime string) (string, []string) {
	prompt := "这是一个微信表情包图片。请把它分类到以下情绪之一: [" + strings.Join(t.cfg.StickerEmotions, ", ") +
		"]，并给出最多5个中文关键词。只输出 JSON: {\"emotion\":\"...\",\"keywords\":[\"...\"]}"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := t.eng.Complete(ctx, runRequest{
		SessionKey: "wx:system:sticker-tag",
		Profile:    t.tagProfile(),
		Prompt:     prompt,
		Image:      img,
		ImageMime:  mime,
	})
	if err != nil {
		return "", nil
	}
	t.store.addUsage(res.Usage, t.tagProfile())
	return parseEmotionJSON(res.Text, t.cfg.StickerEmotions)
}

func parseEmotionJSON(text string, emotions []string) (string, []string) {
	s := strings.TrimSpace(text)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var out struct {
		Emotion  string   `json:"emotion"`
		Keywords []string `json:"keywords"`
	}
	if json.Unmarshal([]byte(s), &out) != nil {
		return "", nil
	}
	for _, e := range emotions {
		if e == out.Emotion {
			return out.Emotion, out.Keywords
		}
	}
	return "其他", out.Keywords
}

// randHex 生成随机令牌（首次启动自动生成 bridge_token 时用）
func randHex(n int) string {
	const hexs = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexs[rand.Intn(len(hexs))]
	}
	return string(b)
}
