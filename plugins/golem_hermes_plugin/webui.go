package main

import (
	"log/slog"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// webUI 监控台：/ui 单页 + /api/* 数据面，复用工具桥进程与端口。
// 鉴权：登录换取 HttpOnly Cookie（12h）；工具桥自身的 Bearer 协议不受影响。
type webUI struct {
	p        *hermesPlugin
	mu       sync.Mutex
	sessions map[string]time.Time
}

func newWebUI(p *hermesPlugin) *webUI {
	return &webUI{p: p, sessions: map[string]time.Time{}}
}

func (w *webUI) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui", w.handleUI)
	mux.HandleFunc("GET /", w.handleUI)
	mux.HandleFunc("POST /api/login", w.handleLogin)
	mux.HandleFunc("POST /api/logout", w.handleLogout)
	mux.HandleFunc("GET /api/overview", w.auth(w.handleOverview))
	mux.HandleFunc("GET /api/runs", w.auth(w.handleRuns))
	mux.HandleFunc("GET /api/decisions", w.auth(w.handleDecisions))
	mux.HandleFunc("GET /api/chats", w.auth(w.handleChats))
	mux.HandleFunc("GET /api/context", w.auth(w.handleContext))
	mux.HandleFunc("GET /api/usage", w.auth(w.handleUsage))
	mux.HandleFunc("GET /api/stickers", w.auth(w.handleStickers))
	mux.HandleFunc("GET /api/stickers/file/", w.auth(w.handleStickerFile))
	mux.HandleFunc("GET /api/logs", w.auth(w.handleLogs))
	mux.HandleFunc("GET /api/config", w.auth(w.handleConfig))
	mux.HandleFunc("POST /api/action/retag", w.auth(w.handleRetag))
	mux.HandleFunc("GET /api/stream", w.auth(w.handleStream))
	mux.HandleFunc("GET /api/members", w.auth(w.handleMemberCards))
	mux.HandleFunc("GET /api/members/", w.auth(w.handleMemberRoutes))
	mux.HandleFunc("POST /api/action/profile-rerun", w.auth(w.handleProfileRerun))
	mux.HandleFunc("GET /api/chat-settings", w.auth(w.handleChatSettingsGet))
	mux.HandleFunc("PUT /api/chat-settings", w.auth(w.handleChatSettingsPut))
	mux.HandleFunc("GET /api/member-flags", w.auth(w.handleMemberFlagsGet))
	mux.HandleFunc("PUT /api/member-flags", w.auth(w.handleMemberFlagsPut))
	mux.HandleFunc("GET /api/chronicle/list", w.auth(w.handleChronicleList))
	mux.HandleFunc("GET /api/chronicle/status", w.auth(w.handleChronicleStatus))
	mux.HandleFunc("GET /api/chronicle/detail", w.auth(w.handleChronicleDetail))
	mux.HandleFunc("GET /api/chronicle/file/", w.auth(w.handleChronicleFile))
	mux.HandleFunc("POST /api/chronicle/publish", w.auth(w.handleChroniclePublish))
	mux.HandleFunc("PUT /api/chronicle/schedule", w.auth(w.handleChronicleSchedule))
	mux.HandleFunc("GET /api/chronicle/styles", w.auth(w.handleChronicleStylesGet))
	mux.HandleFunc("PUT /api/chronicle/styles", w.auth(w.handleChronicleStylesPut))
	mux.HandleFunc("GET /api/imagegen/config", w.auth(w.handleImagegenConfigGet))
	mux.HandleFunc("PUT /api/imagegen/config", w.auth(w.handleImagegenConfigPut))
	mux.HandleFunc("GET /api/gallery", w.auth(w.handleGallery))
	mux.HandleFunc("GET /api/gallery/file/", w.auth(w.handleGalleryFile))
}

func (w *webUI) handleUI(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 磁盘优先：hermes_data/ui.html 存在则用之（改页面无需重编译），否则用内嵌版
	if w.p.dataDir != "" {
		if b, err := os.ReadFile(filepath.Join(w.p.dataDir, "ui.html")); err == nil && len(b) > 0 {
			_, _ = rw.Write(b)
			return
		}
	}
	_, _ = rw.Write(uiHTML)
}

// ---- 鉴权 ----

func (w *webUI) newSession() string {
	b := make([]byte, 24)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	tok := hex.EncodeToString(b)
	w.mu.Lock()
	w.sessions[tok] = time.Now().Add(12 * time.Hour)
	w.mu.Unlock()
	return tok
}

func (w *webUI) valid(r *http.Request) bool {
	c, err := r.Cookie("gohm_ui")
	if err != nil || c.Value == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	exp, ok := w.sessions[c.Value]
	return ok && time.Now().Before(exp)
}

func (w *webUI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.valid(r) {
			writeErr(rw, http.StatusUnauthorized, "未登录")
			return
		}
		next(rw, r)
	}
}

func (w *webUI) handleLogin(rw http.ResponseWriter, r *http.Request) {
	token := w.p.Config.BridgeToken
	if token == "" {
		writeErr(rw, http.StatusServiceUnavailable, "未配置 bridge_token")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(token)) != 1 {
		time.Sleep(500 * time.Millisecond)
		writeErr(rw, http.StatusUnauthorized, "令牌错误")
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name: "gohm_ui", Value: w.newSession(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	writeOK(rw, map[string]any{"login": true})
}

func (w *webUI) handleLogout(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("gohm_ui"); err == nil {
		w.mu.Lock()
		delete(w.sessions, c.Value)
		w.mu.Unlock()
	}
	http.SetCookie(rw, &http.Cookie{Name: "gohm_ui", Value: "", Path: "/", MaxAge: -1})
	writeOK(rw, map[string]any{"logout": true})
}

// ---- 数据面 ----

func (w *webUI) handleOverview(rw http.ResponseWriter, _ *http.Request) {
	p := w.p
	m := p.metrics
	out := map[string]any{
		"version":  pluginVersion,
		"uptime_s": int(time.Since(m.start).Seconds()),
		"health":   m.healthSnapshot(),
		"inflight": m.inflightCount(),
		"workers":  map[string]any{"busy": len(p.disp.inFlightSessions()), "sessions": len(p.disp.activeCopy())},
		"decisions_today": m.decisionStats(),
		"latency": m.latencyStats(),
		"stickers": stickerOverview(p.store),
		"workshop": p.store.workorderStats(),
	}
	tp, tc := p.store.usageToday()
	out["usage_today"] = map[string]any{"prompt": tp, "completion": tc, "cap": p.Config.DailyTokenCap}
	if p.eng != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out["gateway_ok"] = p.eng.Health(ctx) == nil
		cancel()
	} else {
		out["gateway_ok"] = false
	}
	writeOK(rw, out)
}

func (w *webUI) handleRuns(rw http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	writeOK(rw, w.p.metrics.runsSnapshot(limit, r.URL.Query().Get("chat"), r.URL.Query().Get("status")))
}

func (w *webUI) handleDecisions(rw http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 100)
	writeOK(rw, w.p.metrics.decisionsSnapshot(limit))
}

func (w *webUI) handleChats(rw http.ResponseWriter, _ *http.Request) {
	writeOK(rw, w.p.metrics.chatsSnapshot())
}

func (w *webUI) handleContext(rw http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(rw, http.StatusBadRequest, "缺少 key")
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 40)
	writeOK(rw, map[string]any{"key": key, "messages": w.p.rings.snapshot(key, limit)})
}

func (w *webUI) handleUsage(rw http.ResponseWriter, r *http.Request) {
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	writeOK(rw, map[string]any{
		"days":   w.p.store.usageDays(days),
		"today":  w.p.store.usageTodayDetail(),
	})
}

func (w *webUI) handleStickers(rw http.ResponseWriter, r *http.Request) {
	emotion := r.URL.Query().Get("emotion")
	writeOK(rw, map[string]any{
		"by_emotion": w.p.store.stickerByEmotion(),
		"recent":     w.p.store.stickerRecent(emotion, 60),
		"queue":      w.p.store.stickerQueue(),
	})
}

func (w *webUI) handleStickerFile(rw http.ResponseWriter, r *http.Request) {
	md5 := strings.TrimPrefix(r.URL.Path, "/api/stickers/file/")
	data := w.p.store.stickerFile(md5)
	if data == nil {
		writeErr(rw, http.StatusNotFound, "不存在")
		return
	}
	ct := "image/gif"
	switch sniffImage(data) {
	case "jpg":
		ct = "image/jpeg"
	case "png":
		ct = "image/png"
	}
	rw.Header().Set("Content-Type", ct)
	rw.Header().Set("Cache-Control", "max-age=3600")
	_, _ = rw.Write(data)
}

func (w *webUI) handleLogs(rw http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	writeOK(rw, w.p.metrics.logsSnapshot(limit, r.URL.Query().Get("level")))
}

func (w *webUI) handleConfig(rw http.ResponseWriter, _ *http.Request) {
	c := w.p.Config
	mask := func(s string) string {
		if s == "" {
			return "(未配置)"
		}
		if len(s) <= 8 {
			return "***"
		}
		return s[:4] + "****" + s[len(s)-4:]
	}
	writeOK(rw, map[string]any{
		"version": pluginVersion,
		"base_url": c.BaseURL, "api_key": mask(c.APIKey),
		"fast_profile": c.FastProfile, "full_profile": c.FullProfile, "vision_profile": c.VisionProfile,
		"context_window": c.ContextWindow, "ambient_reply_rate": c.AmbientReplyRate,
		"ambient_cooldown_sec": c.AmbientCooldownSec, "free_response_chats": c.FreeResponseChats,
		"owner_only_private": c.OwnerOnlyPrivate, "owner_bypass_ambient": c.OwnerBypassAmbient,
		"group_sessions_per_user": c.GroupSessionsPerUser, "owner_count": len(w.p.ownerSet),
        "crow_enabled": c.CrowEnabled, "grudge_enabled": c.GrudgeEnabled, "mood_enabled": c.MoodEnabled,
        "profile_enabled": c.ProfileEnabled, "chronicle_enabled": c.ChronicleEnabled,
        "chronicle_hour": c.ChronicleHour, "chronicle_minute": c.ChronicleMinute,
		"sticker_enabled": c.StickerEnabled, "sticker_probability": c.StickerProbability,
		"daily_token_cap": c.DailyTokenCap, "bridge_listen": c.BridgeListen,
		"bridge_token": mask(c.BridgeToken), "run_timeout_sec": c.RunTimeoutSec,
		"config_file": "D:\\chatbot\\plugins\\config.toml",
		"hint":        "修改 config.toml 后热生效；/pm reload golem_hermes_plugin 重载代码",
	})
}

func (w *webUI) handleRetag(rw http.ResponseWriter, _ *http.Request) {
	if w.p.store.db == nil {
		writeErr(rw, http.StatusInternalServerError, "存储不可用")
		return
	}
	w.p.store.mu.Lock()
	_, err := w.p.store.db.Exec("UPDATE stickers SET attempts=0 WHERE attempts>=3")
	w.p.store.mu.Unlock()
	if err != nil {
		writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	if w.p.tagger != nil {
		go w.p.tagger.tagBatch()
	}
	w.p.metrics.addLog("INFO", "[web] 触发表情重打标")
	writeOK(rw, map[string]any{"retag": true})
}

// ---- SSE 实时流 ----

func (w *webUI) handleStream(rw http.ResponseWriter, r *http.Request) {
	fl, ok := rw.(http.Flusher)
	if !ok {
		writeErr(rw, http.StatusInternalServerError, "不支持流式")
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			buf := &strings.Builder{}
			enc := json.NewEncoder(buf)
			// 每秒推一个轻量快照（复用 overview 组装，但去掉重探测）
			snap := w.overviewNoProbe()
			_ = enc.Encode(snap)
			fmt.Fprintf(rw, "data: %s\n\n", buf.String())
			fl.Flush()
		}
	}
}

func (w *webUI) overviewNoProbe() map[string]any {
	p := w.p
	m := p.metrics
	tp, tc := p.store.usageToday()
	return map[string]any{
		"version": pluginVersion,
		"uptime_s": int(time.Since(m.start).Seconds()),
		"health": m.healthSnapshot(),
		"inflight": m.inflightCount(),
		"workers": map[string]any{"busy": len(p.disp.inFlightSessions()), "sessions": len(p.disp.activeCopy())},
		"decisions_today": m.decisionStats(),
		"latency": m.latencyStats(),
		"stickers": stickerOverview(p.store),
		"workshop": p.store.workorderStats(),
		"usage_today": map[string]any{"prompt": tp, "completion": tc, "cap": p.Config.DailyTokenCap},
		"ts": time.Now().Unix(),
	}
}

func stickerOverview(s *store) map[string]any {
	total, tagged := s.stickerCounts()
	return map[string]any{"total": total, "tagged": tagged, "queue": total - tagged}
}

// ---- 画像 API ----

var wxidRe = regexp.MustCompile(`^[A-Za-z0-9_@-]{1,64}$`)

func (w *webUI) handleMemberCards(rw http.ResponseWriter, _ *http.Request) {
	writeOK(rw, map[string]any{"members": w.p.store.memberCards()})
}

// handleMemberRoutes /api/members/{wxid} 与 /api/members/avatar/{wxid}
func (w *webUI) handleMemberRoutes(rw http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/members/")
	if rest == "" {
		writeErr(rw, http.StatusBadRequest, "缺少 wxid")
		return
	}
	if strings.HasPrefix(rest, "avatar/") {
		wxid := strings.TrimPrefix(rest, "avatar/")
		if !wxidRe.MatchString(wxid) {
			writeErr(rw, http.StatusBadRequest, "非法标识")
			return
		}
		path := w.p.store.avatarPath(wxid)
		if path == "" {
			writeErr(rw, http.StatusNotFound, "无头像")
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			writeErr(rw, http.StatusNotFound, "无头像")
			return
		}
		ct := "image/jpeg"
		switch sniffImage(data) {
		case "png":
			ct = "image/png"
		case "gif":
			ct = "image/gif"
		}
		rw.Header().Set("Content-Type", ct)
		rw.Header().Set("Cache-Control", "max-age=86400")
		_, _ = rw.Write(data)
		return
	}
	// DELETE /api/members/{wxid} 删除画像
	if r.Method == http.MethodDelete {
		wxid := rest
		if !wxidRe.MatchString(wxid) {
			writeErr(rw, http.StatusBadRequest, "非法标识")
			return
		}
		w.p.store.mu.Lock()
		_, err := w.p.store.db.Exec("DELETE FROM member_profiles WHERE wxid=?", wxid)
		w.p.store.mu.Unlock()
		if err != nil {
			writeErr(rw, http.StatusInternalServerError, err.Error())
			return
		}
		w.p.metrics.addLog("INFO", "[web] 删除成员画像 "+wxid)
		writeOK(rw, map[string]any{"deleted": wxid})
		return
	}
	// GET /api/members/{wxid} 详情
	if !wxidRe.MatchString(rest) {
		writeErr(rw, http.StatusBadRequest, "非法标识")
		return
	}
	d := w.p.store.memberDetailGet(rest, w.p.Config.StickerEmotions)
	if d == nil {
		writeErr(rw, http.StatusNotFound, "暂无画像")
		return
	}
	writeOK(rw, d)
}

// ---- 史官 ----

func (w *webUI) handleChronicleList(rw http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	writeOK(rw, map[string]any{"issues": w.p.store.chronicleList(r.URL.Query().Get("chat"), limit)})
}

func (w *webUI) handleChronicleStatus(rw http.ResponseWriter, _ *http.Request) {
	c := w.p.Config
	chats := []map[string]any{}
	if w.p.chronicle != nil {
		for _, ch := range w.p.chronicle.chronicleChats() {
			chats = append(chats, map[string]any{"chat": ch, "today_msgs": w.p.store.archiveCountSince(ch, time.Now().Format("2006-01-02"))})
		}
	}
	writeOK(rw, map[string]any{
		"enabled": c.ChronicleEnabled, "hour": c.ChronicleHour, "minute": c.ChronicleMinute,
		"extra_threshold": c.ChronicleExtraThreshold, "style_mode": c.ChronicleStyleMode,
		"fixed_style": c.ChronicleFixedStyle, "chats": chats,
	})
}

func (w *webUI) handleChronicleDetail(rw http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.URL.Query().Get("id"), 0))
	e := w.p.store.chronicleGet(id)
	if e == nil {
		writeErr(rw, http.StatusNotFound, "不存在")
		return
	}
	writeOK(rw, e)
}

func (w *webUI) handleChronicleFile(rw http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(strings.TrimPrefix(r.URL.Path, "/api/chronicle/file/"), 0))
	e := w.p.store.chronicleGet(id)
	if e == nil || e.ImgFile == "" {
		writeErr(rw, http.StatusNotFound, "无报纸图")
		return
	}
	data, err := os.ReadFile(e.ImgFile)
	if err != nil {
		writeErr(rw, http.StatusNotFound, "无报纸图")
		return
	}
	rw.Header().Set("Content-Type", "image/png")
	rw.Header().Set("Cache-Control", "max-age=86400")
	_, _ = rw.Write(data)
}

func (w *webUI) handleChroniclePublish(rw http.ResponseWriter, r *http.Request) {
	if w.p.chronicle == nil {
		writeErr(rw, http.StatusServiceUnavailable, "史官未启用")
		return
	}
	var body struct {
		Chat string `json:"chat"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if len(w.p.chronicle.chronicleChats()) == 0 && body.Chat == "" {
		writeErr(rw, http.StatusBadRequest, "没有启用史官的群（会话页⚙开启）")
		return
	}
	id, err := w.p.chronicle.PublishNow(body.Chat)
	if err != nil {
		writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(rw, map[string]any{"published": true, "id": id})
}

// handleChronicleSchedule 出刊计划（时间/阈值/开关）
func (w *webUI) handleChronicleSchedule(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled    *bool `json:"enabled"`
		Hour       *int  `json:"hour"`
		Minute     *int  `json:"minute"`
		Extra     *int  `json:"extra_threshold"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(rw, http.StatusBadRequest, "参数无效")
		return
	}
	if body.Enabled != nil {
		w.p.Config.ChronicleEnabled = *body.Enabled
		if *body.Enabled && w.p.chronicle == nil {
			w.p.chronicle = newChronicle(w.p)
			go w.p.chronicle.loop()
		}
	}
	if body.Hour != nil && *body.Hour >= 0 && *body.Hour <= 23 {
		w.p.Config.ChronicleHour = *body.Hour
	}
	if body.Minute != nil && *body.Minute >= 0 && *body.Minute <= 59 {
		w.p.Config.ChronicleMinute = *body.Minute
	}
	if body.Extra != nil && *body.Extra >= 0 {
		w.p.Config.ChronicleExtraThreshold = *body.Extra
	}
	if err := w.p.SaveConfig(w.p); err != nil {
		slog.Warn("[ghp] 史官配置回写失败", "err", err)
	}
	w.p.metrics.addLog("INFO", "[web] 史官编务配置更新")
	writeOK(rw, map[string]any{"saved": true})
}

func (w *webUI) handleChronicleStylesGet(rw http.ResponseWriter, _ *http.Request) {
	c := &w.p.Config
	usage := map[string]int{}
	for _, e := range w.p.store.chronicleList("", 500) {
		usage[e.Style]++
	}
	styles := []map[string]any{}
	for _, st := range c.ChronicleStyles {
		styles = append(styles, map[string]any{"name": st.Name, "hint": st.Hint, "enabled": st.Enabled, "used": usage[st.Name]})
	}
	writeOK(rw, map[string]any{"styles": styles, "mode": c.ChronicleStyleMode, "fixed": c.ChronicleFixedStyle})
}

// handleChronicleStylesPut 文风坊：整体保存（新增/更新/删除/策略）
func (w *webUI) handleChronicleStylesPut(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode  *string          `json:"mode"`
		Fixed *string          `json:"fixed"`
		Styles []chronicleStyle `json:"styles"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body); err != nil {
		writeErr(rw, http.StatusBadRequest, "参数无效")
		return
	}
	c := &w.p.Config
	if body.Mode != nil && (*body.Mode == "rotate" || *body.Mode == "fixed") {
		c.ChronicleStyleMode = *body.Mode
	}
	if body.Fixed != nil {
		c.ChronicleFixedStyle = *body.Fixed
	}
	if body.Styles != nil {
		if len(body.Styles) == 0 {
			writeErr(rw, http.StatusBadRequest, "文风池不能为空")
			return
		}
		enabled := 0
		for _, st := range body.Styles {
			if st.Enabled {
				enabled++
			}
			if st.Name == "" || strings.TrimSpace(st.Hint) == "" {
				writeErr(rw, http.StatusBadRequest, "文风需要名称与腔调说明")
				return
			}
		}
		if enabled == 0 {
			writeErr(rw, http.StatusBadRequest, "至少保留一个启用的文风")
			return
		}
		c.ChronicleStyles = body.Styles
	}
	if err := w.p.SaveConfig(w.p); err != nil {
		writeErr(rw, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	w.p.metrics.addLog("INFO", "[web] 文风坊更新")
	writeOK(rw, map[string]any{"saved": true})
}

// ---- 画坊（生图/改图） ----

func maskKey(s string) string {
	if s == "" {
		return "(未配置)"
	}
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

func (w *webUI) handleImagegenConfigGet(rw http.ResponseWriter, _ *http.Request) {
	st := w.p.imagegenSettings()
	writeOK(rw, map[string]any{
		"enabled": st.Enabled, "base_url": st.BaseURL, "api_key": maskKey(st.APIKey),
		"model": st.Model, "size": st.Size, "daily_cap": st.DailyCap,
		"dedup_win": st.DedupWinMin, "sim_win": st.SimWinMin,
		"sim_threshold": st.SimThreshold, "peruser_max": st.PerUserMax,
		"workers": w.p.Config.ImageGenWorkers,
	})
}

// handleImagegenConfigPut 服务商配置写 kv（即改即生效）；api_key 传空串=保持不变
func (w *webUI) handleImagegenConfigPut(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  *bool   `json:"enabled"`
		BaseURL  *string `json:"base_url"`
		APIKey   *string `json:"api_key"`
		Model    *string `json:"model"`
		Size     *string `json:"size"`
		DailyCap *int    `json:"daily_cap"`
		// 防复读把关
		DedupWin     *int     `json:"dedup_win"`
		SimWin       *int     `json:"sim_win"`
		SimThreshold *float64 `json:"sim_threshold"`
		PerUserMax   *int     `json:"peruser_max"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeErr(rw, http.StatusBadRequest, "参数无效")
		return
	}
	if body.BaseURL != nil {
		u := strings.TrimRight(strings.TrimSpace(*body.BaseURL), "/")
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeErr(rw, http.StatusBadRequest, "base_url 需以 http(s):// 开头")
			return
		}
		w.p.store.kvSet("imagegen:base_url", u)
	}
	if body.APIKey != nil {
		if k := strings.TrimSpace(*body.APIKey); k != "" && k != "(未配置)" && !strings.Contains(k, "****") {
			w.p.store.kvSet("imagegen:api_key", k)
		}
	}
	if body.Model != nil {
		if m := strings.TrimSpace(*body.Model); m != "" {
			w.p.store.kvSet("imagegen:model", m)
		}
	}
	if body.Size != nil {
		if s := strings.TrimSpace(*body.Size); s != "" {
			w.p.store.kvSet("imagegen:size", s)
		}
	}
	if body.DailyCap != nil && *body.DailyCap >= 0 {
		w.p.store.kvSet("imagegen:daily_cap", fmt.Sprintf("%d", *body.DailyCap))
	}
	if body.DedupWin != nil && *body.DedupWin >= 1 && *body.DedupWin <= 240 {
		w.p.store.kvSet("imagegen:dedup_win", fmt.Sprintf("%d", *body.DedupWin))
	}
	if body.SimWin != nil && *body.SimWin >= 1 && *body.SimWin <= 240 {
		w.p.store.kvSet("imagegen:sim_win", fmt.Sprintf("%d", *body.SimWin))
	}
	if body.SimThreshold != nil && *body.SimThreshold >= 0.5 && *body.SimThreshold <= 1 {
		w.p.store.kvSet("imagegen:sim_threshold", fmt.Sprintf("%.2f", *body.SimThreshold))
	}
	if body.PerUserMax != nil && *body.PerUserMax >= 1 && *body.PerUserMax <= 20 {
		w.p.store.kvSet("imagegen:peruser_max", fmt.Sprintf("%d", *body.PerUserMax))
	}
	if body.Enabled != nil {
		w.p.store.kvSet("imagegen:enabled", map[bool]string{true: "1", false: "0"}[*body.Enabled])
		if *body.Enabled {
			w.p.ensureWorkshop()
		}
	}
	w.p.metrics.addLog("INFO", "[web] 画坊服务商配置更新")
	st := w.p.imagegenSettings()
	writeOK(rw, map[string]any{
		"enabled": st.Enabled, "base_url": st.BaseURL, "api_key": maskKey(st.APIKey),
		"model": st.Model, "size": st.Size, "daily_cap": st.DailyCap,
	})
}

func (w *webUI) handleGallery(rw http.ResponseWriter, r *http.Request) {
	writeOK(rw, map[string]any{
		"stats": w.p.store.workorderStats(),
		"items": w.p.store.listWorkorders(r.URL.Query().Get("chat"), r.URL.Query().Get("status"), atoiDefault(r.URL.Query().Get("limit"), 60)),
	})
}

func (w *webUI) handleGalleryFile(rw http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(strings.TrimPrefix(r.URL.Path, "/api/gallery/file/"), 0))
	wo := w.p.store.getWorkorder(id)
	if wo == nil || wo.FilePath == "" || wo.Status != woStatusDone {
		writeErr(rw, http.StatusNotFound, "无此画作")
		return
	}
	data, err := os.ReadFile(wo.FilePath)
	if err != nil {
		writeErr(rw, http.StatusNotFound, "画作文件丢失")
		return
	}
	ct := "image/png"
	switch sniffImage(data) {
	case "jpg":
		ct = "image/jpeg"
	case "gif":
		ct = "image/gif"
	}
	rw.Header().Set("Content-Type", ct)
	rw.Header().Set("Cache-Control", "max-age=86400")
	_, _ = rw.Write(data)
}

// ---- 群设置 / 成员标记（配置中枢） ----

func (w *webUI) handleChatSettingsGet(rw http.ResponseWriter, r *http.Request) {
	chat := r.URL.Query().Get("chat")
	if chat != "" {
		writeOK(rw, w.p.store.getChatSettings(chat))
		return
	}
	writeOK(rw, map[string]any{"settings": w.p.store.chatSettingsList()})
}

func (w *webUI) handleChatSettingsPut(rw http.ResponseWriter, r *http.Request) {
	var cs chatSettings
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&cs); err != nil || cs.Chat == "" {
		writeErr(rw, http.StatusBadRequest, "参数无效")
		return
	}
	if cs.SessionMode != nil && *cs.SessionMode != "shared" && *cs.SessionMode != "per_user" {
		writeErr(rw, http.StatusBadRequest, "session_mode 仅支持 shared|per_user")
		return
	}
	w.p.store.saveChatSettings(cs)
	w.p.metrics.addLog("INFO", "[web] 群设置更新 "+previewOf(cs.Chat, 40))
	writeOK(rw, w.p.store.getChatSettings(cs.Chat))
}

func (w *webUI) handleMemberFlagsGet(rw http.ResponseWriter, r *http.Request) {
	wxid := r.URL.Query().Get("wxid")
	if wxid == "" {
		writeErr(rw, http.StatusBadRequest, "缺少 wxid")
		return
	}
	writeOK(rw, w.p.store.getMemberFlags(wxid))
}

func (w *webUI) handleMemberFlagsPut(rw http.ResponseWriter, r *http.Request) {
	var mf memberFlags
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&mf); err != nil || mf.Wxid == "" {
		writeErr(rw, http.StatusBadRequest, "参数无效")
		return
	}
	w.p.store.saveMemberFlags(mf)
	writeOK(rw, w.p.store.getMemberFlags(mf.Wxid))
}

func (w *webUI) handleProfileRerun(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Wxid string `json:"wxid"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if body.Wxid != "" {
		if !wxidRe.MatchString(body.Wxid) {
			writeErr(rw, http.StatusBadRequest, "非法标识")
			return
		}
		w.p.store.mu.Lock()
		_, _ = w.p.store.db.Exec("UPDATE member_profiles SET archived_at=0 WHERE wxid=?", body.Wxid)
		w.p.store.mu.Unlock()
	} else {
		w.p.store.mu.Lock()
		_, _ = w.p.store.db.Exec("UPDATE member_profiles SET archived_at=0")
		w.p.store.mu.Unlock()
	}
	if w.p.profiler != nil {
		w.p.profiler.Wake()
	}
	w.p.metrics.addLog("INFO", "[web] 触发画像重跑 "+body.Wxid)
	writeOK(rw, map[string]any{"rerun": true})
}
