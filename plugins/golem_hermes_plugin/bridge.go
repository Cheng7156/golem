package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sbgayhub/golem/sdk/plugin"
)

// bridgeServer 微信能力工具桥：供 hermes 侧 golem_bridge 插件回呼。
// 防越权闸门：请求必须绑定到一个"活跃窗口内"的会话，agent 无法指定任意 wxid。
type bridgeServer struct {
	p   *hermesPlugin
	srv *http.Server
	ln  net.Listener
}

func newBridge(p *hermesPlugin) *bridgeServer {
	b := &bridgeServer{p: p}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/emoji", b.auth(b.handleEmoji))
	mux.HandleFunc("POST /v1/image", b.auth(b.handleImage))
	mux.HandleFunc("POST /v1/quote", b.auth(b.handleQuote))
	mux.HandleFunc("GET /v1/context", b.auth(b.handleContext))
	mux.HandleFunc("POST /v1/history", b.auth(b.handleHistory))
	mux.HandleFunc("GET /v1/members", b.auth(b.handleMembers))
	mux.HandleFunc("POST /v1/remember", b.auth(b.handleRemember))
	mux.HandleFunc("POST /v1/profile", b.auth(b.handleProfile))
	mux.HandleFunc("GET /v1/ping", b.handlePing)
	// 监控台：/ui 单页 + /api/* 数据面（Cookie 会话，与工具桥 Bearer 协议并存）
	w := newWebUI(b.p)
	w.mount(mux)
	b.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return b
}

func (b *bridgeServer) start() {
	ln, err := net.Listen("tcp", b.p.Config.BridgeListen)
	if err != nil {
		slog.Error("[ghp] 工具桥监听失败", "addr", b.p.Config.BridgeListen, "err", err)
		return
	}
	b.ln = ln
	slog.Info("[ghp] 工具桥已启动", "addr", ln.Addr().String())
	if err := b.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("[ghp] 工具桥异常退出", "err", err)
	}
}

func (b *bridgeServer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.srv.Shutdown(ctx)
}

func (b *bridgeServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := b.p.Config.BridgeToken
		if token == "" {
			writeErr(w, http.StatusServiceUnavailable, "工具桥未配置令牌")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "令牌无效")
			return
		}
		next(w, r)
	}
}

// resolveSession 会话绑定（隔离第一要务），按优先级：
//  1. 显式 X-Hermes-Session-Key：必须命中活跃窗口内的会话
//  2. X-Hermes-Session-Id（hermes 内部会话ID）：反查本地会话键，多群并发也能精确绑定
//  3. 未携带任何头：仅当"恰好一个 run 在途"时绑定该会话
//  4. 其余情况：拒绝——绝不猜"最近活跃"
func (b *bridgeServer) resolveSession(r *http.Request) (string, *activeSession, error) {
	window := time.Duration(b.p.Config.ActiveWindowMin) * time.Minute
	if window <= 0 {
		window = 30 * time.Minute
	}
	active := b.p.disp.activeCopy()
	key := strings.TrimSpace(r.Header.Get("X-Hermes-Session-Key"))
	if key != "" {
		as, ok := active[key]
		if !ok || time.Since(as.LastActive) > window {
			return "", nil, errors.New("会话不在活跃窗口内，拒绝操作")
		}
		return key, &as, nil
	}
	// hermes 内部会话ID 反查（golem_bridge 每次调用都会带上）
	if sid := strings.TrimSpace(r.Header.Get("X-Hermes-Session-Id")); sid != "" {
		if ck := b.p.eng.ChatKeyByHermesSession(sid); ck != "" {
			if as, ok := active[ck]; ok && time.Since(as.LastActive) <= window {
				return ck, &as, nil
			}
		}
	}
	inFlight := b.p.disp.inFlightSessions()
	switch len(inFlight) {
	case 1:
		as, ok := b.p.disp.activeCopy()[inFlight[0]]
		if !ok {
			return "", nil, errors.New("在途会话状态异常")
		}
		return inFlight[0], &as, nil
	case 0:
		return "", nil, errors.New("当前无运行中的会话，拒绝操作")
	default:
		return "", nil, fmt.Errorf("有 %d 个会话并发运行且未显式绑定，拒绝操作以防串扰", len(inFlight))
	}
}

func (b *bridgeServer) handleEmoji(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Emotion string `json:"emotion"`
		Keyword string `json:"keyword"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	cooldown := time.Duration(b.p.Config.StickerCooldownMin) * time.Minute
	data := b.p.store.pickSticker(body.Emotion, body.Keyword, cooldown)
	if data == nil {
		writeErr(w, http.StatusNotFound, "没有匹配的表情")
		return
	}
	if err := b.p.out.sendEmoji(as.Receiver, data); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"sent": true, "bytes": len(data)})
}

func (b *bridgeServer) handleImage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string `json:"kind"` // markdown | text
		Content string `json:"content"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	capability := "markdown.to.image"
	if body.Kind == "text" {
		capability = "text.to.image"
	}
	if strings.TrimSpace(body.Content) == "" {
		writeErr(w, http.StatusBadRequest, "content 不能为空")
		return
	}
	_, data, err := callWithTimeout(b.p.caller, capability, map[string]string{"context": body.Content}, 90*time.Second)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "渲染失败: "+err.Error())
		return
	}
	if err := b.p.out.sendImage(as.Receiver, data); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"sent": true, "bytes": len(data)})
}

func (b *bridgeServer) handleQuote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text      string `json:"text"`
		MessageID int64  `json:"message_id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	ref := as.LastTask
	if ref == nil {
		writeErr(w, http.StatusConflict, "没有可引用的入站消息")
		return
	}
	if body.MessageID != 0 && body.MessageID != ref.InMsgID {
		writeErr(w, http.StatusBadRequest, "只能引用当前会话最近一条消息")
		return
	}
	if err := b.p.out.sendQuote(ref, body.Text); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"sent": true})
}

func (b *bridgeServer) handleContext(w http.ResponseWriter, r *http.Request) {
	key, _, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	writeOK(w, map[string]any{"session": key, "messages": b.p.rings.snapshot(key, limit)})
}

func (b *bridgeServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Limit int `json:"limit"`
	}
	_ = readJSON(w, r, &body)
	key, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	limit := body.Limit
	if limit <= 0 {
		limit = 50
	}
	args := map[string]string{"limit": fmt.Sprintf("%d", limit)}
	if as.IsGroup {
		args["chatroom"] = as.GroupID
	} else {
		args["member"] = as.Receiver
	}
	mime, data, err := callWithTimeout(b.p.caller, "statistics.query_messages", args, 60*time.Second)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询历史失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true,"session":%q,"mime":%q,"data":`, key, mime)
	if mime == "json" {
		w.Write(data)
	} else {
		jsonBytes, _ := json.Marshal(string(data))
		w.Write(jsonBytes)
	}
	w.Write([]byte("}"))
}

func (b *bridgeServer) handleMembers(w http.ResponseWriter, r *http.Request) {
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if !as.IsGroup {
		writeErr(w, http.StatusBadRequest, "仅群会话可查询成员")
		return
	}
	members := b.p.chatroom.ListMembers(as.GroupID)
	out := make([]map[string]string, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]string{
			"username":    m.GetUsername(),
			"nickname":    m.GetNickname(),
			"displayname": m.GetDisplayName(),
			"remark":      m.GetRemark(),
		})
	}
	writeOK(w, map[string]any{"group": as.GroupID, "members": out})
}

func (b *bridgeServer) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, map[string]any{"service": "golem_hermes_plugin bridge", "version": pluginVersion})
}

// handleProfile 查询成员画像（只读，仍绑定当前会话）：按昵称/备注模糊匹配
func (b *bridgeServer) handleProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if _, _, err := b.resolveSession(r); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	d := b.p.store.memberSearch(name, b.p.Config.StickerEmotions)
	if d == nil {
		// 给出相近候选，方便 agent 二次确认
		writeOK(w, map[string]any{"found": false, "hint": "没有该成员画像（语料不足或名字不对）"})
		return
	}
	writeOK(w, map[string]any{"found": true, "profile": d})
}

// handleRemember 对话记住：只允许记"当前发言人"（会话绑定，防冒记他人）
func (b *bridgeServer) handleRemember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	t := as.LastTask
	if t == nil || t.SpeakerID == "" {
		writeErr(w, http.StatusConflict, "当前会话没有可识别的发言人")
		return
	}
	chatName := t.GroupID
	if t.IsGroup {
		chatName = "群聊"
	} else {
		chatName = "私聊"
	}
	if b.p.store.memberRemember(t.SpeakerID, t.Speaker, body.Kind, body.Content, chatName) {
		b.p.metrics.addLog("INFO", "[bridge] 记住: "+t.Speaker+" | "+previewOf(body.Content, 50))
		writeOK(w, map[string]any{"remembered": true, "about": t.Speaker})
		return
	}
	writeErr(w, http.StatusBadRequest, "内容为空")
}

// callWithTimeout CallPlugin 无超时语义，统一包一层护栏
func callWithTimeout(c plugin.CallerAbility, capability string, args map[string]string, timeout time.Duration) (string, []byte, error) {
	type result struct {
		mime string
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		m, d, e := c.CallPlugin(capability, args)
		ch <- result{m, d, e}
	}()
	select {
	case r := <-ch:
		return r.mime, r.data, r.err
	case <-time.After(timeout):
		return "", nil, fmt.Errorf("调用 %s 超时(%s)", capability, timeout)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4*1024*1024))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func writeOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
