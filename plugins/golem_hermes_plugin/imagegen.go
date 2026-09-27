package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "image/gif"  // 注册解码器，画稿压缩用
	_ "image/jpeg"
	_ "image/png"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

// ============================================================
// 画坊：AI 生图/改图（全异步工单制）
// 甲方（群友）下工单 → 桥接毫秒级受理 → 后台画手作画 → 交稿发群 / 难产出讣告
// ============================================================

const (
	woStatusQueued   = "queued"
	woStatusPainting = "painting"
	woStatusDone     = "delivered"
	woStatusFailed   = "failed"

	woKindGenerate = "generate"
	woKindEdit     = "edit"

	woQueueCap     = 64
	woPollInterval = 5 * time.Second
	woPollMax      = 5 * time.Minute
	woHintAfter    = 15 * time.Second
	woRingKeep     = 20             // 每群图片环保留张数
	woRingTTL      = 2 * time.Hour  // 图片环素材有效期
)

var paintHints = []string{
	"🖌️ 画师正在调色…",
	"🖌️ 画师正在构思构图…",
	"🖌️ 画师甩了甩手腕，继续…",
	"🖌️ 画师眯起眼睛找灵感…",
}

var failEpitaphs = []string{
	"画师看了一眼需求，跑了",
	"画师画到一半睡着了",
	"颜料用完了，改天再战",
	"这张画出了大问题，画师决定假装没画过",
}

// ---- 运行时配置：kv（Web 可改，即改即生效） > config.toml ----

type imagegenSettings struct {
	Enabled  bool
	BaseURL  string
	APIKey   string
	Model    string
	Size     string
	DailyCap int
	// 防复读把关（仅 kv 配置，Web 画坊卡片可改）
	DedupWinMin  int     // L1 库存复用窗口（分钟）
	SimWinMin    int     // L2 相似拦截窗口（分钟）
	SimThreshold float64 // L2 相似阈值（bigram Jaccard）
	PerUserMax   int     // L3 同甲方 10 分钟内最大单数
}

func (p *hermesPlugin) imagegenSettings() imagegenSettings {
	st := imagegenSettings{
		Enabled:  p.Config.ImageGenEnabled,
		BaseURL:  strings.TrimRight(strings.TrimSpace(p.Config.ImageGenBaseURL), "/"),
		APIKey:   strings.TrimSpace(p.Config.ImageGenAPIKey),
		Model:    p.Config.ImageGenModel,
		Size:     p.Config.ImageGenSize,
		DailyCap: p.Config.ImageGenDailyCap,
	}
	if p.store != nil {
		if v := p.store.kvGet("imagegen:enabled"); v != "" {
			st.Enabled = v == "1"
		}
		if v := strings.TrimSpace(p.store.kvGet("imagegen:base_url")); v != "" {
			st.BaseURL = strings.TrimRight(v, "/")
		}
		if v := strings.TrimSpace(p.store.kvGet("imagegen:api_key")); v != "" {
			st.APIKey = v
		}
		if v := strings.TrimSpace(p.store.kvGet("imagegen:model")); v != "" {
			st.Model = v
		}
		if v := strings.TrimSpace(p.store.kvGet("imagegen:size")); v != "" {
			st.Size = v
		}
		if v := strings.TrimSpace(p.store.kvGet("imagegen:daily_cap")); v != "" {
			st.DailyCap = atoiDefault(v, st.DailyCap)
		}
	}
	if st.Model == "" {
		st.Model = "gpt-image-2.5"
	}
	if st.Size == "" {
		st.Size = "auto"
	}
	st.DedupWinMin = 30
	st.SimWinMin = 10
	st.SimThreshold = 0.80 // 单字替换破坏2个bigram：J=(N-3)/(N+1)，0.8 → 核心串≥19字才拦单字改动
	st.PerUserMax = 3
	if p.store != nil {
		if v := p.store.kvGet("imagegen:dedup_win"); v != "" {
			if n := atoiDefault(v, 30); n > 0 {
				st.DedupWinMin = n
			}
		}
		if v := p.store.kvGet("imagegen:sim_win"); v != "" {
			if n := atoiDefault(v, 10); n > 0 {
				st.SimWinMin = n
			}
		}
		if v := p.store.kvGet("imagegen:sim_threshold"); v != "" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0.5 && f <= 1 {
				st.SimThreshold = f
			}
		}
		if v := p.store.kvGet("imagegen:peruser_max"); v != "" {
			if n := atoiDefault(v, 3); n > 0 {
				st.PerUserMax = n
			}
		}
	}
	return st
}

// imagegenAllow 配额闸：总开关 → 服务商 → 群开关 → 全局/会话双层日产能（主人免检）
func (p *hermesPlugin) imagegenAllow(chat string, isOwner bool) (bool, string) {
	st := p.imagegenSettings()
	if !st.Enabled {
		return false, "画坊未开张"
	}
	if st.BaseURL == "" || st.APIKey == "" {
		return false, "画坊还没签约画师（服务商未配置）"
	}
	if isOwner {
		return true, ""
	}
	ec := p.effectiveChatConfig(chat)
	if !ec.ImagegenEnabled {
		return false, "本群未开画坊"
	}
	day := time.Now().Format("20060102")
	if st.DailyCap > 0 && p.store.quotaPeek("g:"+day) >= st.DailyCap {
		return false, "今日画坊产能已售罄，甲方明天请早"
	}
	if ec.ImagegenDailyCap > 0 && p.store.quotaPeek("c:"+day+":"+chat) >= ec.ImagegenDailyCap {
		return false, "本群今日画坊额度已用完"
	}
	return true, ""
}

// ---- 工单 ----

type workorder struct {
	ID          int64  `json:"id"`
	Chat        string `json:"chat"`
	ChatName    string `json:"chat_name"`
	Requester   string `json:"requester"`
	RequesterID string `json:"requester_id"`
	Kind        string `json:"kind"`
	Prompt      string `json:"prompt"`
	Ref         string `json:"ref"`
	RefMsgID    int64  `json:"ref_msg_id"`
	Size        string `json:"size"`
	Status      string `json:"status"`
	TaskID      string `json:"task_id"`
	FilePath    string `json:"file_path"`
	Error       string `json:"error"`
	CreatedAt   int64  `json:"created_at"`
	DoneAt      int64  `json:"done_at"`
}

func (s *store) initImagegen() {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS workorders(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat TEXT, chat_name TEXT DEFAULT '', requester TEXT DEFAULT '', requester_id TEXT DEFAULT '',
		kind TEXT DEFAULT 'generate', prompt TEXT DEFAULT '', ref TEXT DEFAULT '', ref_msg_id INTEGER DEFAULT 0,
		size TEXT DEFAULT '', status TEXT DEFAULT 'queued', task_id TEXT DEFAULT '', file_path TEXT DEFAULT '',
		error TEXT DEFAULT '', created_at INTEGER, done_at INTEGER DEFAULT 0)`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_wo_status ON workorders(status, id)`)
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS image_ring(
		msg_id INTEGER PRIMARY KEY, chat TEXT, wxid TEXT DEFAULT '', name TEXT DEFAULT '',
		md5 TEXT DEFAULT '', aeskey TEXT DEFAULT '', size INTEGER DEFAULT 0, created_at INTEGER)`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_ring_chat ON image_ring(chat, created_at)`)
}

func (s *store) createWorkorder(wo *workorder) int64 {
	if s == nil || s.db == nil {
		return 0
	}
	wo.CreatedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO workorders(chat,chat_name,requester,requester_id,kind,prompt,ref,ref_msg_id,size,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		wo.Chat, wo.ChatName, wo.Requester, wo.RequesterID, wo.Kind, wo.Prompt, wo.Ref, wo.RefMsgID, wo.Size, woStatusQueued, wo.CreatedAt)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	wo.ID = id
	return id
}

func (s *store) getWorkorder(id int64) *workorder {
	if s == nil || s.db == nil || id <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wo := &workorder{}
	err := s.db.QueryRow(`SELECT id,chat,COALESCE(chat_name,''),COALESCE(requester,''),COALESCE(requester_id,''),
		kind,prompt,COALESCE(ref,''),ref_msg_id,COALESCE(size,''),status,COALESCE(task_id,''),COALESCE(file_path,''),
		COALESCE(error,''),created_at,done_at FROM workorders WHERE id=?`, id).
		Scan(&wo.ID, &wo.Chat, &wo.ChatName, &wo.Requester, &wo.RequesterID,
			&wo.Kind, &wo.Prompt, &wo.Ref, &wo.RefMsgID, &wo.Size, &wo.Status, &wo.TaskID, &wo.FilePath,
			&wo.Error, &wo.CreatedAt, &wo.DoneAt)
	if err != nil {
		return nil
	}
	return wo
}

func (s *store) updateWorkorder(wo *workorder) {
	if s == nil || s.db == nil || wo == nil || wo.ID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE workorders SET status=?,task_id=?,file_path=?,error=?,done_at=? WHERE id=?`,
		wo.Status, wo.TaskID, wo.FilePath, wo.Error, wo.DoneAt, wo.ID)
}

func (s *store) workordersByStatus(statuses []string, limit int) []*workorder {
	out := []*workorder{}
	if s == nil || s.db == nil || len(statuses) == 0 {
		return out
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(statuses)), ",")
	args := make([]any, 0, len(statuses)+1)
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, limit)
	s.mu.Lock()
	rows, err := s.db.Query("SELECT id FROM workorders WHERE status IN ("+ph+") ORDER BY id ASC LIMIT ?", args...)
	if err != nil {
		s.mu.Unlock()
		return out
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	s.mu.Unlock()
	for _, id := range ids {
		if wo := s.getWorkorder(id); wo != nil {
			out = append(out, wo)
		}
	}
	return out
}

func (s *store) listWorkorders(chat, status string, limit int) []*workorder {
	out := []*workorder{}
	if s == nil || s.db == nil {
		return out
	}
	if limit <= 0 {
		limit = 60
	}
	q := `SELECT id,chat,COALESCE(chat_name,''),COALESCE(requester,''),kind,prompt,COALESCE(ref,''),
		COALESCE(size,''),status,COALESCE(task_id,''),COALESCE(file_path,''),COALESCE(error,''),created_at,done_at
		FROM workorders WHERE 1=1`
	var args []any
	if chat != "" {
		q += " AND chat=?"
		args = append(args, chat)
	}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		wo := &workorder{}
		if rows.Scan(&wo.ID, &wo.Chat, &wo.ChatName, &wo.Requester, &wo.Kind, &wo.Prompt, &wo.Ref,
			&wo.Size, &wo.Status, &wo.TaskID, &wo.FilePath, &wo.Error, &wo.CreatedAt, &wo.DoneAt) == nil {
			if wo.FilePath != "" {
				wo.FilePath = fmt.Sprintf("/api/gallery/file/%d", wo.ID)
			}
			out = append(out, wo)
		}
	}
	return out
}

// workordersInWindow 某群 since 之后创建的工单（查重用，FilePath 保持本机磁盘路径不转 URL）
func (s *store) workordersInWindow(chat string, since int64, limit int) []*workorder {
	out := []*workorder{}
	if s == nil || s.db == nil || chat == "" {
		return out
	}
	if limit <= 0 {
		limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id,chat,COALESCE(chat_name,''),COALESCE(requester,''),COALESCE(requester_id,''),
		kind,prompt,COALESCE(ref,''),ref_msg_id,COALESCE(size,''),status,COALESCE(task_id,''),COALESCE(file_path,''),
		COALESCE(error,''),created_at,done_at
		FROM workorders WHERE chat=? AND created_at>=? ORDER BY id DESC LIMIT ?`, chat, since, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		wo := &workorder{}
		if rows.Scan(&wo.ID, &wo.Chat, &wo.ChatName, &wo.Requester, &wo.RequesterID,
			&wo.Kind, &wo.Prompt, &wo.Ref, &wo.RefMsgID, &wo.Size, &wo.Status, &wo.TaskID, &wo.FilePath,
			&wo.Error, &wo.CreatedAt, &wo.DoneAt) == nil {
			out = append(out, wo)
		}
	}
	return out
}

// workorderStats 画坊 KPI：今日交稿/难产、在画/排队、平均耗时
func (s *store) workorderStats() map[string]any {
	out := map[string]any{"today_done": 0, "today_failed": 0, "painting": 0, "queued": 0, "avg_sec": 0}
	if s == nil || s.db == nil {
		return out
	}
	midnight := time.Now().Truncate(24 * time.Hour).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM workorders WHERE status=? AND done_at>=?", woStatusDone, midnight).Scan(&n)
	out["today_done"] = n
	_ = s.db.QueryRow("SELECT COUNT(*) FROM workorders WHERE status=? AND done_at>=?", woStatusFailed, midnight).Scan(&n)
	out["today_failed"] = n
	_ = s.db.QueryRow("SELECT COUNT(*) FROM workorders WHERE status=?", woStatusPainting).Scan(&n)
	out["painting"] = n
	_ = s.db.QueryRow("SELECT COUNT(*) FROM workorders WHERE status=?", woStatusQueued).Scan(&n)
	out["queued"] = n
	var avg float64
	_ = s.db.QueryRow("SELECT AVG(done_at-created_at) FROM workorders WHERE status=? AND done_at>=?", woStatusDone, midnight).Scan(&avg)
	out["avg_sec"] = int(avg)
	return out
}

// ---- 图片环（改图素材来源：只存元数据，用时才下载原图） ----

type ringImage struct {
	MsgID     int64
	Chat      string
	Wxid      string
	Name      string
	Md5       string
	AesKey    string
	Size      uint32
	CreatedAt int64
}

func (s *store) ringAddImage(im ringImage) {
	if s == nil || s.db == nil || im.Chat == "" || im.Md5 == "" {
		return
	}
	im.CreatedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT OR REPLACE INTO image_ring(msg_id,chat,wxid,name,md5,aeskey,size,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		im.MsgID, im.Chat, im.Wxid, im.Name, im.Md5, im.AesKey, im.Size, im.CreatedAt)
	_, _ = s.db.Exec("DELETE FROM image_ring WHERE chat=? AND created_at<?", im.Chat, time.Now().Add(-woRingTTL).Unix())
	_, _ = s.db.Exec(`DELETE FROM image_ring WHERE chat=? AND msg_id NOT IN
		(SELECT msg_id FROM image_ring WHERE chat=? ORDER BY created_at DESC, msg_id DESC LIMIT ?)`,
		im.Chat, im.Chat, woRingKeep)
}

func (s *store) ringLatest(chat string) (ringImage, bool) {
	return s.ringQuery("SELECT msg_id,chat,wxid,name,md5,aeskey,size,created_at FROM image_ring WHERE chat=? ORDER BY created_at DESC, msg_id DESC LIMIT 1", chat)
}

func (s *store) ringBySender(chat, nameOrWxid string) (ringImage, bool) {
	q := nameOrWxid
	return s.ringQuery(`SELECT msg_id,chat,wxid,name,md5,aeskey,size,created_at FROM image_ring
		WHERE chat=? AND (wxid=? OR name=? OR name LIKE ?)
		ORDER BY created_at DESC, msg_id DESC LIMIT 1`, chat, q, q, q+"%")
}

func (s *store) ringByMsgID(chat string, msgID int64) (ringImage, bool) {
	return s.ringQuery("SELECT msg_id,chat,wxid,name,md5,aeskey,size,created_at FROM image_ring WHERE chat=? AND msg_id=?", chat, msgID)
}

func (s *store) ringQuery(q string, args ...any) (ringImage, bool) {
	im := ringImage{}
	if s == nil || s.db == nil {
		return im, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var size int64
	err := s.db.QueryRow(q, args...).Scan(&im.MsgID, &im.Chat, &im.Wxid, &im.Name, &im.Md5, &im.AesKey, &size, &im.CreatedAt)
	if err != nil {
		return im, false
	}
	im.Size = uint32(size)
	return im, true
}

// ---- 配额计数（kv：imgcap:<g|c>:<day>[:chat]） ----

func (s *store) quotaPeek(scope string) int {
	if s == nil {
		return 0
	}
	return atoiDefault(s.kvGet("imgcap:"+scope), 0)
}

func (s *store) quotaIncr(scope string) int {
	if s == nil || s.db == nil {
		return 0
	}
	k := "imgcap:" + scope
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur int
	_ = s.db.QueryRow("SELECT v FROM kv WHERE k=?", k).Scan(&cur)
	cur++
	_, _ = s.db.Exec("INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, fmt.Sprintf("%d", cur))
	return cur
}

// ---- 服务商客户端（生图/查单/改图/下载，响应结构宽容解析） ----

type imagegenClient struct {
	hc   *http.Client
	base string // 形如 https://host/v1
	root string // 形如 https://host（查单端点 /api/image-tasks 挂在根上）
	key  string
}

func newImagegenClient(baseURL, apiKey string) *imagegenClient {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	root := strings.TrimSuffix(base, "/v1")
	return &imagegenClient{
		hc:   &http.Client{},
		base: base,
		root: root,
		key:  apiKey,
	}
}

func (c *imagegenClient) do(ctx context.Context, method, url string, ct string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	return data, resp.StatusCode, err
}

// imageResult 一张画：task_id=需继续轮询；url/b64=已成稿
type imageResult struct {
	TaskID string
	URL    string
	B64    string
}

func (r *imageResult) ready() bool { return r.URL != "" || r.B64 != "" }

// submitAsync 提交异步生图，返回 task_id（若服务商直接返回成稿也能接住）
func (c *imagegenClient) submitAsync(prompt, size, idemKey, model string) (*imageResult, error) {
	payload := map[string]any{
		"model": model, "prompt": prompt, "n": 1, "size": size,
		"async": true, "response_format": "url",
	}
	blob, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/images/generations", bytes.NewReader(blob))
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	return parseImageResponse(data, resp.StatusCode)
}

// editImage 参考图改图：multipart 上传本地图片。可能同步出稿，也可能受理为任务
func (c *imagegenClient) editImage(prompt, imagePath, size, idemKey, model string) (*imageResult, error) {
	img, err := os.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("打开素材失败: %w", err)
	}
	defer img.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("prompt", prompt)
	_ = mw.WriteField("n", "1")
	_ = mw.WriteField("response_format", "url")
	if size != "" && size != "auto" {
		_ = mw.WriteField("size", size)
	}
	part, err := mw.CreateFormFile("image", filepath.Base(imagePath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, img); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	return parseImageResponse(data, resp.StatusCode)
}

// parseImageResponse 宽容解析：task_id 受理回执 / OpenAI 风格 data[0].url|b64_json / 504 带 task_id
func parseImageResponse(data []byte, code int) (*imageResult, error) {
	if len(data) == 0 {
		if code == http.StatusGatewayTimeout {
			return nil, errors.New("504 且无 task_id，画师失联")
		}
		return nil, fmt.Errorf("服务商响应为空 (HTTP %d)", code)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("响应解析失败 (HTTP %d): %s", code, truncateRunes(string(data), 160))
	}
	res := &imageResult{}
	// 任务受理形态：task_id / id / task.id / data[0].task_id
	res.TaskID = digString(top, "task_id", "taskId")
	if res.TaskID == "" {
		if id := digString(top, "id"); id != "" && (strings.HasPrefix(id, "api_gen") || code == http.StatusGatewayTimeout || top["data"] == nil) {
			res.TaskID = id
		}
	}
	if res.TaskID == "" {
		if task, ok := top["task"].(map[string]any); ok {
			res.TaskID = digString(task, "id", "task_id")
		}
	}
	// 成稿形态：data[0].url / data[0].b64_json / url / b64_json
	if arr, ok := top["data"].([]any); ok && len(arr) > 0 {
		if item, ok := arr[0].(map[string]any); ok {
			res.URL = digString(item, "url")
			res.B64 = digString(item, "b64_json")
			if res.TaskID == "" {
				res.TaskID = digString(item, "task_id", "taskId")
			}
		}
	}
	if !res.ready() {
		res.URL = digString(top, "url")
		res.B64 = digString(top, "b64_json")
	}
	if !res.ready() && res.TaskID == "" {
		if msg := digString(top, "message", "error.message", "error", "detail"); msg != "" {
			return nil, fmt.Errorf("服务商错误 (HTTP %d): %s", code, truncateRunes(msg, 160))
		}
		return nil, fmt.Errorf("无法识别的响应 (HTTP %d): %s", code, truncateRunes(string(data), 160))
	}
	return res, nil
}

// queryTask 查询异步任务：返回 (normalized status, 结果, err)。status: painting|done|failed
func (c *imagegenClient) queryTask(taskID string) (string, *imageResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/api/image-tasks?ids=%s", c.root, taskID)
	data, code, err := c.do(ctx, http.MethodGet, url, "", nil)
	if err != nil {
		return "painting", nil, err // 网络抖动按"还在画"处理，交给轮询上限兜底
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return "failed", nil, fmt.Errorf("服务商鉴权失败 (HTTP %d)，画师辞职了", code)
	}
	if code != http.StatusOK {
		return "painting", nil, fmt.Errorf("查单 HTTP %d", code)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return "painting", nil, fmt.Errorf("查单响应解析失败: %s", truncateRunes(string(data), 160))
	}
	var items []any
	for _, key := range []string{"tasks", "items", "data", "results"} {
		if arr, ok := top[key].([]any); ok {
			items = arr
			break
		}
	}
	if items == nil {
		if arr, ok := top["task"].([]any); ok {
			items = arr
		} else if one, ok := top["task"].(map[string]any); ok {
			items = []any{one}
		}
	}
	if len(items) == 0 {
		return "painting", nil, nil
	}
	item, _ := items[0].(map[string]any)
	if item == nil {
		return "painting", nil, nil
	}
	status := strings.ToLower(digString(item, "status", "state"))
	switch {
	case strings.Contains(status, "fail"), strings.Contains(status, "error"), strings.Contains(status, "cancel"):
		return "failed", nil, errors.New(digString(item, "public_error", "error.message", "error", "message", "fail_reason"))
	case strings.Contains(status, "succ"), strings.Contains(status, "complet"), strings.Contains(status, "done"),
		strings.Contains(status, "finish"), strings.Contains(status, "ok"):
		return "done", taskResult(item), nil
	default: // pending/running/queued/processing/空
		return "painting", nil, nil
	}
}

// taskResult 从任务条目里挖成稿（results/images/output 数组，url 或 b64_json）
func taskResult(item map[string]any) *imageResult {
	res := &imageResult{}
	if u := digString(item, "url", "result_url", "image_url"); u != "" {
		res.URL = u
		return res
	}
	if r, ok := item["result"].(map[string]any); ok {
		if u := digString(r, "url"); u != "" {
			res.URL = u
			return res
		}
	}
	for _, key := range []string{"results", "images", "output", "data"} {
		arr, ok := item[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		switch first := arr[0].(type) {
		case string:
			res.URL = first
			return res
		case map[string]any:
			res.URL = digString(first, "url")
			res.B64 = digString(first, "b64_json")
			if res.ready() {
				return res
			}
		}
	}
	return res
}

// digString 依次尝试多层键名（支持 "a.b" 路径）
func digString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		cur := m
		parts := strings.Split(key, ".")
		ok := true
		for i, p := range parts {
			if i == len(parts)-1 {
				if v, exists := cur[p]; exists {
					if s, isStr := v.(string); isStr && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
				break
			}
			next, exists := cur[p]
			if !exists {
				ok = false
				break
			}
			if cur, ok = next.(map[string]any); !ok {
				break
			}
		}
		_ = ok
	}
	return ""
}

func (c *imagegenClient) download(url string) ([]byte, error) {
	if strings.HasPrefix(url, "/") { // 服务商返回相对路径时按根地址补全
		url = c.root + url
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, code, err := c.do(ctx, http.MethodGet, url, "", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("下载画作失败 HTTP %d", code)
	}
	if len(data) < 512 {
		return nil, errors.New("下载的画作小得可疑，丢弃")
	}
	return data, nil
}

// ---- 画坊引擎：后台画手 ----

type workshop struct {
	p     *hermesPlugin
	queue chan int64
	stop  chan struct{}

	mu      sync.Mutex
	running bool
	hinted  map[int64]bool
}

func newWorkshop(p *hermesPlugin) *workshop {
	return &workshop{p: p, queue: make(chan int64, woQueueCap), stop: make(chan struct{}), hinted: map[int64]bool{}}
}

// ensureWorkshop 按需启动（幂等）；OnLoad 与 Web 开启画坊时都会走这里
func (p *hermesPlugin) ensureWorkshop() {
	if p.workshop == nil {
		p.workshop = newWorkshop(p)
	}
	p.workshop.start()
}

func (ws *workshop) start() {
	ws.mu.Lock()
	if ws.running {
		ws.mu.Unlock()
		return
	}
	ws.running = true
	ws.mu.Unlock()
	workers := ws.p.Config.ImageGenWorkers
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		go ws.worker()
	}
	// 续单：reload/重启后 queued 重新入队，painting 拿 task_id 续查
	resumed := 0
	for _, wo := range ws.p.store.workordersByStatus([]string{woStatusQueued, woStatusPainting}, 50) {
		ws.submit(wo.ID)
		resumed++
	}
	slog.Info("[画坊] 开张", "workers", workers, "resumed", resumed)
	if resumed > 0 {
		ws.p.metrics.addLog("INFO", fmt.Sprintf("[画坊] 开张，续接 %d 张在途工单", resumed))
	}
}

func (ws *workshop) Stop() {
	ws.mu.Lock()
	if !ws.running {
		ws.mu.Unlock()
		return
	}
	ws.running = false
	close(ws.stop)
	ws.mu.Unlock()
}

// submit 非阻塞入队；爆单则当场难产
func (ws *workshop) submit(id int64) {
	select {
	case ws.queue <- id:
	default:
		if wo := ws.p.store.getWorkorder(id); wo != nil {
			ws.fail(wo, "画坊爆单了（队列已满）")
		}
	}
}

func (ws *workshop) worker() {
	for {
		select {
		case <-ws.stop:
			return
		case id := <-ws.queue:
			ws.process(id)
		}
	}
}

func (ws *workshop) process(id int64) {
	p := ws.p
	wo := p.store.getWorkorder(id)
	if wo == nil || wo.Status == woStatusDone || wo.Status == woStatusFailed {
		return
	}
	st := p.imagegenSettings()
	if st.BaseURL == "" || st.APIKey == "" {
		ws.fail(wo, "画坊还没签约画师（服务商未配置）")
		return
	}
	ws.mu.Lock()
	delete(ws.hinted, wo.ID)
	ws.mu.Unlock()
	wo.Status = woStatusPainting
	p.store.updateWorkorder(wo)
	p.metrics.addLog("INFO", fmt.Sprintf("[画坊] 工单#%d 开画（%s）甲方:%s", wo.ID, wo.Kind, wo.Requester))

	client := newImagegenClient(st.BaseURL, st.APIKey)
	size := wo.Size
	if size == "" {
		size = st.Size
	}
	var res *imageResult
	var err error
	if wo.Kind == woKindEdit {
		res, err = ws.doEdit(client, wo, st, size)
	} else {
		res, err = ws.doGenerate(client, wo, st, size)
	}
	if err != nil {
		ws.fail(wo, err.Error())
		return
	}
	data, err := ws.fetchArtwork(client, res)
	if err != nil {
		ws.fail(wo, err.Error())
		return
	}
	dir := filepath.Join(p.dataDir, "gallery")
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if werr := os.WriteFile(filepath.Join(dir, fmt.Sprintf("wo-%d.png", wo.ID)), data, 0o644); werr == nil {
			wo.FilePath = filepath.Join(dir, fmt.Sprintf("wo-%d.png", wo.ID))
		}
	}
	wo.Status = woStatusDone
	wo.DoneAt = time.Now().Unix()
	p.store.updateWorkorder(wo)
	p.metrics.addLog("INFO", fmt.Sprintf("[画坊] 工单#%d 交稿，耗时 %ds", wo.ID, wo.DoneAt-wo.CreatedAt))
	ws.deliver(wo, data)
}

func (ws *workshop) doGenerate(client *imagegenClient, wo *workorder, st imagegenSettings, size string) (*imageResult, error) {
	res, err := client.submitAsync(wo.Prompt, size, fmt.Sprintf("gohm-wo-%d", wo.ID), st.Model)
	if err != nil {
		return nil, fmt.Errorf("接单失败: %w", err)
	}
	if res.ready() {
		return res, nil
	}
	if res.TaskID == "" {
		return nil, errors.New("服务商未返回任务号")
	}
	wo.TaskID = res.TaskID
	ws.p.store.updateWorkorder(wo)
	return ws.poll(client, wo)
}

func (ws *workshop) doEdit(client *imagegenClient, wo *workorder, st imagegenSettings, size string) (*imageResult, error) {
	im, ok := ws.findMaterial(wo)
	if !ok {
		return nil, errors.New("找不到素材：原图可能已过期（素材只保留2小时）或昵称对不上")
	}
	data, err := ws.downloadMaterial(im)
	if err != nil {
		return nil, fmt.Errorf("下载素材失败: %w", err)
	}
	ext := sniffImage(data)
	if ext == "" {
		return nil, errors.New("素材不是可识别的图片")
	}
	if err := os.MkdirAll(filepath.Join(ws.p.dataDir, "gallery"), 0o755); err != nil {
		return nil, err
	}
	tmp := filepath.Join(ws.p.dataDir, "gallery", fmt.Sprintf("ref-%d.%s", wo.ID, ext))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	res, err := client.editImage(wo.Prompt, tmp, size, fmt.Sprintf("gohm-wo-%d", wo.ID), st.Model)
	if err != nil {
		return nil, fmt.Errorf("改图提交失败: %w", err)
	}
	if res.ready() {
		return res, nil
	}
	if res.TaskID == "" {
		return nil, errors.New("服务商未返回任务号")
	}
	wo.TaskID = res.TaskID
	ws.p.store.updateWorkorder(wo)
	return ws.poll(client, wo)
}

func (ws *workshop) findMaterial(wo *workorder) (ringImage, bool) {
	s := ws.p.store
	switch {
	case wo.RefMsgID != 0:
		return s.ringByMsgID(wo.Chat, wo.RefMsgID)
	case wo.Ref != "":
		return s.ringBySender(wo.Chat, strings.TrimPrefix(strings.TrimSpace(wo.Ref), "@"))
	default:
		return s.ringLatest(wo.Chat)
	}
}

// downloadMaterial 用最小重建消息走宿主 Download（只需 Md5/Key/Size）
func (ws *workshop) downloadMaterial(im ringImage) ([]byte, error) {
	if ws.p.message == nil {
		return nil, errors.New("消息通道未就绪")
	}
	msg := ws.p.buildRingMessage(im)
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		rc, err := ws.p.message.Download(msg)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, 16*1024*1024))
		ch <- result{data: data, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if len(r.data) < 512 {
			return nil, errors.New("素材为空")
		}
		return r.data, nil
	case <-time.After(60 * time.Second):
		return nil, errors.New("下载素材超时")
	}
}

func (ws *workshop) poll(client *imagegenClient, wo *workorder) (*imageResult, error) {
	deadline := time.Now().Add(woPollMax)
	start := time.Now()
	for {
		status, art, err := client.queryTask(wo.TaskID)
		if err != nil {
			if status == "failed" {
				return nil, err
			}
			slog.Warn("[画坊] 查单抖动，继续轮询", "wo", wo.ID, "err", err)
		} else {
			switch status {
			case "done":
				if art == nil || !art.ready() {
					return nil, errors.New("画好了但没找到画（结果缺失）")
				}
				return art, nil
			case "failed":
				return nil, errors.New("服务商返回作画失败")
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("轮询超时，画师失联了")
		}
		if time.Since(start) > woHintAfter {
			ws.hint(wo)
		}
		select {
		case <-ws.stop:
			return nil, errors.New("画坊打烊，工单挂起（重开后自动续画）")
		case <-time.After(woPollInterval):
		}
	}
}

// hint 作画超过 15 秒发一次摸鱼播报（每张工单至多一次）
func (ws *workshop) hint(wo *workorder) {
	ws.mu.Lock()
	if ws.hinted[wo.ID] {
		ws.mu.Unlock()
		return
	}
	ws.hinted[wo.ID] = true
	ws.mu.Unlock()
	if o := ws.p.out; o == nil || o.msg == nil {
		return
	}
	_ = ws.p.out.sendText(wo.Chat, paintHints[int(wo.ID)%len(paintHints)])
}

func (ws *workshop) fetchArtwork(client *imagegenClient, res *imageResult) ([]byte, error) {
	if res.B64 != "" {
		if data, err := base64.StdEncoding.DecodeString(res.B64); err == nil && len(data) > 512 {
			return data, nil
		}
	}
	if res.URL == "" {
		return nil, errors.New("既没有 URL 也没有画稿数据")
	}
	data, err := client.download(res.URL)
	if err != nil {
		return nil, err
	}
	if sniffImage(data) == "" {
		return nil, errors.New("拿回来的东西不是图片，丢弃")
	}
	return data, nil
}

// deliver 交稿：图片一条 + 落款一条
func (ws *workshop) deliver(wo *workorder, data []byte) {
	if o := ws.p.out; o == nil || o.msg == nil {
		slog.Info("[画坊] 无出站通道，跳过交付", "wo", wo.ID)
		return
	}
	send := shrinkImage(data)
	if len(send) > maxImageBytes {
		ws.fail(wo, "画作太大发不进微信")
		return
	}
	if err := ws.p.out.sendImage(wo.Chat, send); err != nil {
		ws.fail(wo, "交付失败: "+err.Error())
		return
	}
	cost := int64(0)
	if wo.DoneAt > wo.CreatedAt {
		cost = wo.DoneAt - wo.CreatedAt
	}
	line := fmt.Sprintf("🎨《%s》画坊工单#%d · 甲方 @%s · 耗时 %ds",
		truncateRunes(wo.Prompt, 14), wo.ID, wo.Requester, cost)
	_ = ws.p.out.sendText(wo.Chat, line)
}

// fail 难产：落库 + 讣告；鉴权失败加发一次 owner 告警（每天最多一次）
func (ws *workshop) fail(wo *workorder, reason string) {
	wo.Status = woStatusFailed
	wo.Error = truncateRunes(reason, 200)
	wo.DoneAt = time.Now().Unix()
	ws.p.store.updateWorkorder(wo)
	ws.p.metrics.addLog("WARN", fmt.Sprintf("[画坊] 工单#%d 难产: %s", wo.ID, reason))
	if o := ws.p.out; o != nil && o.msg != nil {
		epitaph := failEpitaphs[int(wo.ID+wo.DoneAt)%len(failEpitaphs)]
		_ = ws.p.out.sendText(wo.Chat, fmt.Sprintf("💀 工单#%d 难产——%s（工单已入画廊墓志铭）", wo.ID, epitaph))
	}
	if strings.Contains(reason, "辞职") || strings.Contains(reason, "鉴权") {
		day := time.Now().Format("20060102")
		if ws.p.store.kvGet("imagegen:authalert") != day {
			ws.p.store.kvSet("imagegen:authalert", day)
			ws.p.metrics.addLog("ERROR", "[画坊] 服务商鉴权失败，生图已不可用，请到监控台检查 api_key")
			if ws.p.owner != nil && ws.p.owner.GetUsername() != "" && ws.p.out != nil && ws.p.out.msg != nil {
				_ = ws.p.out.sendText(ws.p.owner.GetUsername(), "[画坊] 服务商鉴权失败，请检查生图 api_key")
			}
		}
	}
}

// ---- 防复读把关：归一化 + bigram Jaccard（纯本地计算，零 LLM） ----

// promptFillers 语气填充词：前缀类从头部反复剥，后缀类从尾部反复剥（只动两端，不动中间——中间剥离会把不同需求误融）
var promptPrefixFillers = []string{"帮我画一张", "帮我画个", "帮我画", "帮我来一张", "画一张", "画一幅", "画个", "来一张", "来个", "再画一张", "再画个", "再画", "再来一张", "帮我", "给我", "麻烦", "请", "再", "又"}
var promptSuffixFillers = []string{"可以吗", "好吗", "谢谢", "一下", "一张", "一个", "吧", "呗", "啊", "呢", "啦", "咯", "哈"}

var nonWordRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// normalizePrompt 归一化：全角→半角、小写、去标点/emoji/空白、剥两端填充词、截断 200 字。
// 输出用于 L1 精确比较与 L2 bigram 相似度的“核心串”。
func normalizePrompt(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E: // 全角 ASCII → 半角
			r -= 0xFEE0
		case r == 0x3000: // 全角空格
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s = nonWordRe.ReplaceAllString(b.String(), "")
	ra := []rune(s)
	if len(ra) > 200 {
		ra = ra[:200]
	}
	s = string(ra)
	for changed := true; changed; {
		changed = false
		for _, f := range promptPrefixFillers {
			if strings.HasPrefix(s, f) && len(s) > len([]rune(f)) {
				s = strings.TrimPrefix(s, f)
				changed = true
			}
		}
		for _, f := range promptSuffixFillers {
			if strings.HasSuffix(s, f) && len(s) > len([]rune(f)) {
				s = strings.TrimSuffix(s, f)
				changed = true
			}
		}
	}
	return s
}

// bigramJaccard 相邻字符二元组的 Jaccard 相似度（rune 级，CJK 友好）。
// 任一核心串长度 <2 时退化为精确比较（单字句模糊判定必然乱杀）。
func bigramJaccard(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		if len(ra) == len(rb) && len(ra) > 0 && a == b {
			return 1
		}
		return 0
	}
	setA := map[string]struct{}{}
	for i := 0; i+1 < len(ra); i++ {
		setA[string(ra[i:i+2])] = struct{}{}
	}
	setB := map[string]struct{}{}
	for i := 0; i+1 < len(rb); i++ {
		setB[string(rb[i:i+2])] = struct{}{}
	}
	inter := 0
	for g := range setA {
		if _, ok := setB[g]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// nzSize 尺寸归一（空串视为 auto），保证 key 比较口径一致
func nzSize(s string) string {
	if s == "" || s == "auto" {
		return "auto"
	}
	return s
}

// imagegenDedupe 四层查重把关（在配额闸之前执行）：
//   - L1 精确重复：归一化后与窗口内工单全等 → 生图类返回库存直发（0 元不计配额）；在途单返回 busy
//   - L2 高相似：与「已交稿」工单 bigram Jaccard ≥ 阈值（10 分钟窗口）→ 拒绝（failed 单不计，重试应被允许）
//   - L3 同甲限频：同甲方 10 分钟内第 N 单起拒绝
//
// 返回 (action, 命中工单, 给 LLM 的转告语)；action: ""=放行 | reuse | busy | reject
func (p *hermesPlugin) imagegenDedupe(chat, requesterID string, kind, prompt, size string) (string, *workorder, string) {
	st := p.imagegenSettings()
	core := normalizePrompt(prompt)
	now := time.Now().Unix()
	l1Cutoff := now - int64(st.DedupWinMin)*60
	l2Cutoff := now - int64(st.SimWinMin)*60
	perUser := 0
	for _, wo := range p.store.workordersInWindow(chat, l1Cutoff, 50) {
		live := wo.Status == woStatusQueued || wo.Status == woStatusPainting
		spent := wo.Status == woStatusDone
		if !live && !spent {
			continue // 难产单不参与查重：服务商侧失败的重试应当被允许
		}
		if requesterID != "" && wo.RequesterID == requesterID {
			perUser++
		}
		if wo.Kind != kind || nzSize(wo.Size) != nzSize(size) {
			continue
		}
		same := normalizePrompt(wo.Prompt) == core
		if same && live {
			return "busy", wo, "这张画师正在画，画好自动到，别催"
		}
		// 仅生图类做库存直发：改图素材随时会换，复用旧稿会张冠李戴
		if same && spent && kind == woKindGenerate && wo.FilePath != "" {
			return "reuse", wo, ""
		}
		if spent && wo.CreatedAt >= l2Cutoff && bigramJaccard(normalizePrompt(wo.Prompt), core) >= st.SimThreshold {
			return "reject", wo, fmt.Sprintf("画师拒绝复读：与 %d 分钟前的工单#%d 高度相似，换个说法再来", (now-wo.CreatedAt)/60, wo.ID)
		}
	}
	if perUser >= st.PerUserMax {
		return "reject", nil, "甲方你的需求画师记在小本本上了，歇会儿再来"
	}
	return "", nil, ""
}

// ---- 杂项 ----

// shrinkImage 超过微信 3MB 安全线时转 JPEG 压一压（stdlib 解码重编码）
func shrinkImage(data []byte) []byte {
	if len(data) <= 2800*1024 {
		return data
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return data
	}
	if buf.Len() < len(data) {
		return buf.Bytes()
	}
	return data
}

func truncateRunes(s string, n int) string {
	ra := []rune(strings.TrimSpace(s))
	if len(ra) <= n {
		return strings.TrimSpace(s)
	}
	return string(ra[:n]) + "…"
}

// buildRingMessage 用图片环元数据最小重建消息（宿主 Download 只需 receiver + media 三元组）
func (p *hermesPlugin) buildRingMessage(im ringImage) *message.Message {
	return &message.Message{
		Type:     message.TypeImage,
		Receiver: &contact.Contact{Username: im.Chat},
		Data: &message.Message_Image{Image: &message.ImageData{
			Media: &message.Media{Md5: im.Md5, Key: im.AesKey, Size: im.Size},
		}},
	}
}
