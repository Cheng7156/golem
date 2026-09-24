package main

import (
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
