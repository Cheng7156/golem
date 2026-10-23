package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	mux.HandleFunc("POST /v1/imagegen", b.auth(b.handleImagegen))
	mux.HandleFunc("POST /v1/quote", b.auth(b.handleQuote))
	mux.HandleFunc("GET /v1/context", b.auth(b.handleContext))
	mux.HandleFunc("POST /v1/history", b.auth(b.handleHistory))
	mux.HandleFunc("GET /v1/members", b.auth(b.handleMembers))
	mux.HandleFunc("POST /v1/remember", b.auth(b.handleRemember))
	mux.HandleFunc("POST /v1/profile", b.auth(b.handleProfile))
	mux.HandleFunc("POST /v1/profile_card", b.auth(b.handleProfileCard))
	mux.HandleFunc("POST /v1/video", b.auth(b.handleVideo))
	mux.HandleFunc("POST /v1/deliver", b.auth(b.handleDeliver))
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
		if he, ok := b.p.eng.(*httpEngine); ok {
			if ck := he.ChatKeyByHermesSession(sid); ck != "" {
				if as, ok := active[ck]; ok && time.Since(as.LastActive) <= window {
					return ck, &as, nil
				}
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
		if strings.Contains(err.Error(), "冷却") {
			writeErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
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

// handleImagegen 画坊接单：毫秒级受理（查闸→落工单→播报→回执），作画全在后台画手
func (b *bridgeServer) handleImagegen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt   string `json:"prompt"`
		Ref      string `json:"ref"`
		Size     string `json:"size"`
		RefMsgID int64  `json:"ref_msg_id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		writeErr(w, http.StatusBadRequest, "prompt 不能为空")
		return
	}
	if len([]rune(prompt)) > 2000 {
		prompt = truncateRunes(prompt, 2000)
	}
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	t := as.LastTask
	if t == nil {
		writeErr(w, http.StatusConflict, "当前会话没有可识别的甲方")
		return
	}
	chat := as.Receiver
	// 类型与素材指定（先于查重：查重口径依赖 kind/size）
	kind := woKindGenerate
	ref := strings.TrimSpace(body.Ref)
	refMsgID := body.RefMsgID
	if ref != "" || refMsgID != 0 {
		kind = woKindEdit
		// "quoted" → 从当前入站消息的引用块取被引用消息ID
		if strings.EqualFold(ref, "quoted") && refMsgID == 0 {
			if t != nil {
				fmt.Sscanf(strings.TrimSpace(t.QuoteSvrid), "%d", &refMsgID)
			}
			if refMsgID == 0 {
				writeErr(w, http.StatusBadRequest, "当前消息没有可引用的图片素材")
				return
			}
			ref = ""
		}
	}
	size := strings.TrimSpace(body.Size)

	// 硬闸一：群里 @ 了别人（没 @ 我）的画图需求不是对我下的单，画师不接。
	// 提示词拦不住热心（免@群里 LLM 看到上下文有画图需求就顺手接），必须在接单口判。
	if t.MentionedOther {
		writeErr(w, http.StatusForbidden, "这条消息在 @ 别人，不是对我下的单，画师不接。请如实告知对方：画坊只接直接下达给机器人医生（当前对话）的工单")
		return
	}
	// 硬闸二：ambient 顺路单默认关（环境参与的会话不许烧画坊的钱）
	if t.Ambient && !b.p.imagegenSettings().Ambient {
		writeErr(w, http.StatusForbidden, "画坊未开通顺路单（闲聊参与时不接画图工单）")
		return
	}

	// 防复读把关（先于配额：L1 库存直发零成本，产能售罄也照发）
	if action, match, why := b.p.imagegenDedupe(chat, t.SpeakerID, kind, prompt, size); action != "" {
		if action == "reuse" { // 仅生图类会返回 reuse
			if data, rerr := os.ReadFile(match.FilePath); rerr == nil {
				if o := b.p.out; o != nil && o.msg != nil {
					_ = o.sendImage(chat, shrinkImage(data))
					_ = o.sendText(chat, fmt.Sprintf("📦【画坊】画师拒绝重复劳动——此单有存货（原工单#%d），库存直发，本次 0 元", match.ID))
				}
				b.p.metrics.addLog("INFO", fmt.Sprintf("[画坊] 库存直发 原工单#%d 甲方:%s", match.ID, t.Speaker))
				writeOK(w, map[string]any{
					"accepted": true, "workorder": match.ID, "reused": true,
					"note": "该需求与近期成作完全相同，已直接重发原画（未产生新消耗）。请告诉甲方画坊按库存发货，无需等待。",
				})
				return
			}
			// 存货文件丢失 → 落到正常下单
		} else if action == "busy" {
			writeErr(w, http.StatusConflict, why)
			return
		} else { // reject
			writeErr(w, http.StatusTooManyRequests, why)
			return
		}
	}
	if allowed, why := b.p.imagegenAllow(chat, t.IsOwner); !allowed {
		writeErr(w, http.StatusTooManyRequests, why)
		return
	}
	wo := &workorder{
		Chat: chat, Requester: t.Speaker, RequesterID: t.SpeakerID,
		Kind: kind, Prompt: prompt, Ref: ref, RefMsgID: refMsgID,
		Size: size,
	}
	if id := b.p.store.createWorkorder(wo); id == 0 {
		writeErr(w, http.StatusInternalServerError, "工单落库失败")
		return
	}
	if !t.IsOwner {
		day := time.Now().Format("20060102")
		b.p.store.quotaIncr("g:" + day)
		b.p.store.quotaIncr("c:" + day + ":" + chat)
	}
	b.p.ensureWorkshop()
	b.p.workshop.submit(wo.ID)
	b.p.metrics.addLog("INFO", fmt.Sprintf("[画坊] 接单#%d (%s) 甲方:%s 需求:%s", wo.ID, kind, wo.Requester, previewOf(prompt, 40)))
	if o := b.p.out; o != nil && o.msg != nil {
		note := "画师酝酿中…"
		if kind == woKindEdit {
			note = "画师拿到原图，开始动刀…"
		}
		_ = o.sendText(chat, fmt.Sprintf("🖌️【画坊】工单#%d 已接单，%s", wo.ID, note))
	}
	writeOK(w, map[string]any{
		"accepted": true, "workorder": wo.ID, "kind": kind,
		"note":     "画好后自动发到群里，无需再调用任何工具，也无需向用户索取图片",
	})
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

// handleVideo 视频发送：agent 只递直链，下载与全部硬闸在 golem 侧（确定性代码）。
// 闸门：scheme 白名单 → Content-Length 预检 → 限长流式下载（超限即弃）→ 类型粗检 → 日配额。
func (b *bridgeServer) handleVideo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL      string `json:"url"`
		Duration uint32 `json:"duration"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	rawURL := strings.TrimSpace(body.URL)
	if rawURL == "" {
		writeErr(w, http.StatusBadRequest, "url 不能为空")
		return
	}
	if _, err := url.Parse(rawURL); err != nil {
		writeErr(w, http.StatusBadRequest, "url 无效: "+err.Error())
		return
	}
	_, as, err := b.resolveSession(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	t := as.LastTask
	if t == nil {
		writeErr(w, http.StatusConflict, "当前会话没有可识别的甲方")
		return
	}
	chat := t.Receiver
	maxBytes := b.videoMaxBytes()
	// 日配额（kv 权威账本，reload 不丢）
	if cap := b.p.Config.VideoDailyCap; cap > 0 {
		today := time.Now().Format("20060102")
		if n, _ := strconv.Atoi(b.p.store.kvGet("vidcap:" + chat + ":" + today)); n >= cap {
			writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("本群今日视频配额已用完（%d 条），明天再来", cap))
			return
		}
	}
	data, ok := b.fetchVideo(w, r, rawURL, maxBytes)
	if !ok {
		return
	}
	duration := body.Duration
	if duration == 0 || duration > 600 {
		duration = 15 // 无法探测时长时的保守占位；agent 知道真实时长时应传
	}
	if err := b.p.out.sendVideo(chat, data, placeholderThumb(), duration); err != nil {
		writeErr(w, http.StatusInternalServerError, "发送失败: "+err.Error())
		return
	}
	b.bumpVideoQuota(chat, len(data))
	b.p.metrics.addLog("INFO", fmt.Sprintf("[视频] %s 发送 %.0fKB <- %s", previewOf(chat, 20), float64(len(data))/1024, previewOf(rawURL, 40)))
	writeOK(w, map[string]any{
		"sent": true, "bytes": len(data), "duration": duration,
		"note": "视频已发到群；缩略图为本插件生成的占位图（无 ffmpeg 解帧）",
	})
}

// videoMaxBytes 单条视频字节上限（0=默认 3MB）
func (b *bridgeServer) videoMaxBytes() int {
	if n := b.p.Config.VideoMaxBytes; n > 0 {
		return n
	}
	return maxVideoBytes
}

// bumpVideoQuota 日配额 +1（kv 权威账本）
func (b *bridgeServer) bumpVideoQuota(chat string, _ int) {
	today := time.Now().Format("20060102")
	kvKey := "vidcap:" + chat + ":" + today
	n, _ := strconv.Atoi(b.p.store.kvGet(kvKey))
	b.p.store.kvSet(kvKey, strconv.Itoa(n+1))
}

// fetchVideo 限长下载视频：超过上限一个字节即整体作废（绝不半截发送）。
// 失败时已写好 HTTP 错误响应并返回 false。
func (b *bridgeServer) fetchVideo(w http.ResponseWriter, r *http.Request, rawURL string, maxBytes int) ([]byte, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, http.StatusBadRequest, "url 必须是 http/https 直链")
		return nil, false
	}
	client := &http.Client{Timeout: time.Duration(b.p.Config.VideoTimeoutSec) * time.Second}
	if client.Timeout <= 0 {
		client.Timeout = 30 * time.Second
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "url 无效: "+err.Error())
		return nil, false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) ghp-video/1.0")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "下载失败: "+err.Error())
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("下载源返回 %d", resp.StatusCode))
		return nil, false
	}
	if resp.ContentLength > int64(maxBytes) {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("视频 %.1fMB 超过上限 %.1fMB（gRPC 通道限制，只支持小视频）",
				float64(resp.ContentLength)/1024/1024, float64(maxBytes)/1024/1024))
		return nil, false
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "video") && !strings.Contains(ct, "octet-stream") && !strings.Contains(ct, "mp4") {
		writeErr(w, http.StatusUnsupportedMediaType, "内容类型不是视频: "+ct)
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "下载中断: "+err.Error())
		return nil, false
	}
	if len(data) > maxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("视频超过上限 %.1fMB", float64(maxBytes)/1024/1024))
		return nil, false
	}
	if len(data) < 10*1024 {
		writeErr(w, http.StatusUnprocessableEntity, "下载内容过小，疑似无效视频")
		return nil, false
	}
	return data, true
}

// handleDeliver 定时投递：cron 等无会话任务把内容送进微信群。
// 防越权不变式：不做会话绑定，改走目的地白名单——只有主人圈定的群能收，
// agent 无法投递到任意 wxid。kind=text 直发；kind=video 走限长下载管线。
func (b *bridgeServer) handleDeliver(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Chat     string `json:"chat"`
		Kind     string `json:"kind"`
		Text     string `json:"text"`
		URL      string `json:"url"`
		Duration uint32 `json:"duration"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	chat := strings.TrimSpace(body.Chat)
	if chat == "" {
		writeErr(w, http.StatusBadRequest, "chat 不能为空")
		return
	}
	allowed := false
	for _, g := range b.p.Config.DeliverGroups {
		if g == chat {
			allowed = true
			break
		}
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "目的地不在投递白名单（deliver_to_groups）内，已拒绝")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	if kind == "" {
		kind = "text"
	}
	switch kind {
	case "text":
		text := strings.TrimSpace(body.Text)
		if text == "" {
			writeErr(w, http.StatusBadRequest, "text 不能为空")
			return
		}
		if err := b.p.out.sendText(chat, text); err != nil {
			writeErr(w, http.StatusInternalServerError, "发送失败: "+err.Error())
			return
		}
		b.p.metrics.addLog("INFO", "[定时投递] 文本 -> "+previewOf(chat, 20))
		writeOK(w, map[string]any{"sent": true, "kind": "text"})
	case "video":
		rawURL := strings.TrimSpace(body.URL)
		if rawURL == "" {
			writeErr(w, http.StatusBadRequest, "kind=video 时 url 不能为空")
			return
		}
		if cap := b.p.Config.VideoDailyCap; cap > 0 {
			today := time.Now().Format("20060102")
			if n, _ := strconv.Atoi(b.p.store.kvGet("vidcap:" + chat + ":" + today)); n >= cap {
				writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("本群今日视频配额已用完（%d 条），明天再来", cap))
				return
			}
		}
		data, ok := b.fetchVideo(w, r, rawURL, b.videoMaxBytes())
		if !ok {
			return
		}
		duration := body.Duration
		if duration == 0 || duration > 600 {
			duration = 15
		}
		if err := b.p.out.sendVideo(chat, data, placeholderThumb(), duration); err != nil {
			writeErr(w, http.StatusInternalServerError, "发送失败: "+err.Error())
			return
		}
		b.bumpVideoQuota(chat, len(data))
		b.p.metrics.addLog("INFO", fmt.Sprintf("[定时投递] 视频 %.0fKB -> %s", float64(len(data))/1024, previewOf(chat, 20)))
		writeOK(w, map[string]any{"sent": true, "kind": "video", "bytes": len(data)})
	default:
		writeErr(w, http.StatusBadRequest, "kind 仅支持 text|video")
	}
}

// placeholderThumb 生成 64x64 深灰 JPEG 占位缩略图（无 ffmpeg 依赖；微信列表页显示灰块）
func placeholderThumb() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			img.Set(x, y, color.RGBA{R: 40, G: 40, B: 44, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70})
	return buf.Bytes()
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

// handleProfileCard 一站式画像卡：Go 侧确定性渲染（与监控台画像页同源数据），
// 不给 LLM 拼内容的机会——展示完整性不押在模型自觉上
func (b *bridgeServer) handleProfileCard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if _, as, err := b.resolveSession(r); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	} else {
		// 快路径：后台已预渲染的缓存卡（毫秒级）
		if d := b.p.store.memberSearch(name, b.p.Config.StickerEmotions); d != nil {
			if full := b.p.store.memberDetailGet(d.Wxid, b.p.Config.StickerEmotions); full != nil &&
				full.CardFile != "" {
				if data, rerr := os.ReadFile(full.CardFile); rerr == nil && len(data) > 512 {
					if serr := b.p.out.sendImage(as.Receiver, data); serr == nil {
						writeOK(w, map[string]any{"sent": true, "bytes": len(data), "cached": true})
						return
					}
				}
			}
		}
		// 慢路径兜底：无缓存现场渲染（新成员/文件丢失）
		full := b.p.store.memberSearch(name, b.p.Config.StickerEmotions)
		var md string
		if full != nil {
			md = b.p.store.profileCardMarkdown(b.p.store.memberDetailGet(full.Wxid, b.p.Config.StickerEmotions))
		}
		if md == "" {
			writeOK(w, map[string]any{"found": false, "hint": "查无此人（语料不足或名字不对），如实转告即可，不要编造"})
			return
		}
		_, data, err := callWithTimeout(b.p.caller, "markdown.to.image", map[string]string{"context": md}, 90*time.Second)
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
