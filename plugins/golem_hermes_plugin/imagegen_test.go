package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	s := newStore(filepath.Join(t.TempDir(), "state.db"), filepath.Join(t.TempDir(), "files"))
	t.Cleanup(s.close)
	return s
}

func TestWorkorderCRUD(t *testing.T) {
	s := newTestStore(t)
	wo := &workorder{Chat: "g@chatroom", Requester: "张三", RequesterID: "wxid_z",
		Kind: woKindGenerate, Prompt: "赛博红薯", Size: "auto"}
	id := s.createWorkorder(wo)
	if id == 0 {
		t.Fatal("createWorkorder 返回 0")
	}
	got := s.getWorkorder(id)
	if got == nil || got.Status != woStatusQueued || got.Prompt != "赛博红薯" || got.CreatedAt == 0 {
		t.Fatalf("工单读取异常: %+v", got)
	}
	got.Status = woStatusDone
	got.FilePath = filepath.Join("gallery", fmt.Sprintf("wo-%d.png", id))
	got.DoneAt = got.CreatedAt + 42
	s.updateWorkorder(got)
	got2 := s.getWorkorder(id)
	if got2.Status != woStatusDone || got2.DoneAt-got2.CreatedAt != 42 {
		t.Fatalf("工单更新未生效: %+v", got2)
	}
	// 状态过滤 + 列表
	if n := len(s.workordersByStatus([]string{woStatusQueued}, 10)); n != 0 {
		t.Fatalf("queued 应为 0，得 %d", n)
	}
	if n := len(s.workordersByStatus([]string{woStatusDone}, 10)); n != 1 {
		t.Fatalf("delivered 应为 1，得 %d", n)
	}
	items := s.listWorkorders("", woStatusDone, 10)
	if len(items) != 1 || items[0].FilePath == "" || !strings.HasPrefix(items[0].FilePath, "/api/gallery/file/") {
		t.Fatalf("列表异常: %+v", items)
	}
	st := s.workorderStats()
	if st["today_done"].(int) != 1 {
		t.Fatalf("KPI today_done 应为 1: %v", st)
	}
	if st["avg_sec"].(int) != 42 {
		t.Fatalf("KPI avg_sec 应为 42: %v", st)
	}
}

func TestImageRing(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 25; i++ {
		s.ringAddImage(ringImage{MsgID: int64(100 + i), Chat: "g@chatroom",
			Wxid: fmt.Sprintf("wxid_%d", i%3), Name: fmt.Sprintf("成员%d", i%3),
			Md5: fmt.Sprintf("md5_%d", i), Size: 1024})
	}
	// 环容量：每群最多 woRingKeep 张
	var n int
	s.mu.Lock()
	_ = s.db.QueryRow("SELECT COUNT(*) FROM image_ring WHERE chat='g@chatroom'").Scan(&n)
	s.mu.Unlock()
	if n != woRingKeep {
		t.Fatalf("图片环应保留 %d 张，实际 %d", woRingKeep, n)
	}
	// latest
	if im, ok := s.ringLatest("g@chatroom"); !ok || im.Md5 != "md5_24" {
		t.Fatalf("ringLatest 异常: %+v ok=%v", im, ok)
	}
	// bySender：精确昵称与 wxid
	if im, ok := s.ringBySender("g@chatroom", "成员1"); !ok || im.Wxid != "wxid_1" {
		t.Fatalf("ringBySender(昵称) 异常: %+v", im)
	}
	if im, ok := s.ringBySender("g@chatroom", "wxid_2"); !ok || im.Name != "成员2" {
		t.Fatalf("ringBySender(wxid) 异常: %+v", im)
	}
	// byMsgID + 不存在
	if im, ok := s.ringByMsgID("g@chatroom", 124); !ok || im.Md5 != "md5_24" {
		t.Fatalf("ringByMsgID 异常: %+v", im)
	}
	if _, ok := s.ringByMsgID("g@chatroom", 99999); ok {
		t.Fatal("不存在的 msg_id 不应命中")
	}
	if _, ok := s.ringLatest("other@chatroom"); ok {
		t.Fatal("其他群不应命中")
	}
}

func TestQuotaCounter(t *testing.T) {
	s := newTestStore(t)
	if got := s.quotaPeek("g:20260927"); got != 0 {
		t.Fatalf("初始配额应为 0，得 %d", got)
	}
	for i := 1; i <= 3; i++ {
		if got := s.quotaIncr("g:20260927"); got != i {
			t.Fatalf("第 %d 次incr应返回 %d，得 %d", i, i, got)
		}
	}
	if got := s.quotaPeek("g:20260927"); got != 3 {
		t.Fatalf("peek 应为 3，得 %d", got)
	}
}

func TestImagegenSettingsKvOverride(t *testing.T) {
	s := newTestStore(t)
	p := &hermesPlugin{store: s}
	p.Config.ImageGenEnabled = false
	p.Config.ImageGenBaseURL = "http://cfg-default/v1"
	p.Config.ImageGenAPIKey = "cfg-key"
	p.Config.ImageGenDailyCap = 20

	st := p.imagegenSettings()
	if st.Enabled || st.BaseURL != "http://cfg-default/v1" || st.DailyCap != 20 {
		t.Fatalf("无 kv 时应回落 config: %+v", st)
	}
	s.kvSet("imagegen:enabled", "1")
	s.kvSet("imagegen:base_url", "https://image.mlgb7.com/v1")
	s.kvSet("imagegen:api_key", "kv-key")
	s.kvSet("imagegen:daily_cap", "7")
	st = p.imagegenSettings()
	if !st.Enabled || st.BaseURL != "https://image.mlgb7.com/v1" || st.APIKey != "kv-key" || st.DailyCap != 7 {
		t.Fatalf("kv 覆盖未生效: %+v", st)
	}
}

func TestImagegenAllow(t *testing.T) {
	s := newTestStore(t)
	p := &hermesPlugin{store: s}
	p.Config.ImageGenEnabled = true
	p.Config.ImageGenBaseURL = "https://x.example/v1"
	p.Config.ImageGenAPIKey = "k"

	// 全局开张 → 无覆盖的群默认可下单
	if ok, why := p.imagegenAllow("g@chatroom", false); !ok {
		t.Fatalf("全局开张时无覆盖群应放行: %s", why)
	}
	// 主人 VIP：跳过一切
	if ok, _ := p.imagegenAllow("g@chatroom", true); !ok {
		t.Fatal("主人应免检放行")
	}
	// 会话级覆盖：关
	imgOff := false
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", ImagegenEnabled: &imgOff})
	if ok, why := p.imagegenAllow("g@chatroom", false); ok || why == "" {
		t.Fatalf("群级关闭应拒绝: %v %s", ok, why)
	}
	// 会话级覆盖：开
	imgOn := true
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", ImagegenEnabled: &imgOn})
	if ok, why := p.imagegenAllow("g@chatroom", false); !ok {
		t.Fatalf("群级开启应放行: %s", why)
	}
	// 会话级产能
	cap2 := 2
	s.saveChatSettings(chatSettings{Chat: "g@chatroom", ImagegenEnabled: &imgOn, ImagegenDailyCap: &cap2})
	s.quotaIncr("c:" + time.Now().Format("20060102") + ":g@chatroom")
	s.quotaIncr("c:" + time.Now().Format("20060102") + ":g@chatroom")
	if ok, why := p.imagegenAllow("g@chatroom", false); ok || why == "" {
		t.Fatalf("超会话产能应拒绝: %v %s", ok, why)
	}
	// 服务商未配置：谁都不放行（主人也绕不过没画师的画坊，接单必难产）
	p2 := &hermesPlugin{store: s}
	p2.Config.ImageGenEnabled = true
	if ok, why := p2.imagegenAllow("g@chatroom", false); ok || why == "" {
		t.Fatalf("未配置服务商应拒绝: %v %s", ok, why)
	}
	if ok, _ := p2.imagegenAllow("g@chatroom", true); ok {
		t.Fatal("未配置服务商时主人也应拒绝")
	}
}

// TestParseImageResponse 覆盖常见响应形态
func TestParseImageResponse(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		code    int
		wantTID string
		wantURL string
		wantB64 string
		wantErr bool
	}{
		{"异步受理-task_id", `{"task_id":"api_gen_1","status":"queued"}`, 200, "api_gen_1", "", "", false},
		{"异步受理-id", `{"id":"api_gen_2"}`, 200, "api_gen_2", "", "", false},
		{"异步受理-task.id", `{"task":{"id":"api_gen_3"}}`, 200, "api_gen_3", "", "", false},
		{"504带task_id", `{"task_id":"api_gen_4"}`, 504, "api_gen_4", "", "", false},
		{"同步-url", `{"created":1,"data":[{"url":"http://x/a.png"}]}`, 200, "", "http://x/a.png", "", false},
		{"同步-b64", `{"data":[{"b64_json":"AAAA"}]}`, 200, "", "", "AAAA", false},
		{"错误信息", `{"error":{"message":"bad prompt"}}`, 400, "", "", "", true},
		{"乱响应", `not-json`, 200, "", "", "", true},
	}
	for _, c := range cases {
		res, err := parseImageResponse([]byte(c.body), c.code)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: 期望报错", c.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.TaskID != c.wantTID || res.URL != c.wantURL || res.B64 != c.wantB64 {
			t.Fatalf("%s: got %+v", c.name, res)
		}
	}
}

// newMockProvider 模拟服务商：提交→task_id，查询→轮询 n 次后 done，画作为 1x1 png
func newMockProvider(t *testing.T, pollsBeforeDone int) (*httptest.Server, *[]string) {
	t.Helper()
	polls := 0
	requests := &[]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, "submit")
		if r.Header.Get("Idempotency-Key") == "" {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, `{"task_id":"api_gen_test"}`)
	})
	mux.HandleFunc("GET /api/image-tasks", func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, "query")
		polls++
		if polls <= pollsBeforeDone {
			fmt.Fprint(w, `{"tasks":[{"id":"api_gen_test","status":"running"}]}`)
			return
		}
		fmt.Fprint(w, `{"tasks":[{"id":"api_gen_test","status":"success","url":"/art.png"}]}`)
	})
	mux.HandleFunc("GET /art.png", func(w http.ResponseWriter, _ *http.Request) {
		*requests = append(*requests, "art")
		// 1x1 png 头部合法 + 尾部补白，过 512 字节下载下限
		w.Write(append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
			0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06,
			0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
			0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D,
			0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82},
			make([]byte, 600)...))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, requests
}

func TestImagegenClientEndToEnd(t *testing.T) {
	srv, _ := newMockProvider(t, 1)
	c := newImagegenClient(srv.URL+"/v1", "test-key")
	// 提交
	res, err := c.submitAsync("赛博红薯", "auto", "idem-1", "test-model")
	if err != nil || res.TaskID != "api_gen_test" {
		t.Fatalf("submit 异常: %+v err=%v", res, err)
	}
	// 第一次查询：还在画
	status, art, err := c.queryTask(res.TaskID)
	if err != nil || status != "painting" || art != nil {
		t.Fatalf("首轮查询应 painting: %s %+v %v", status, art, err)
	}
	// 第二次：完成带 url
	status, art, err = c.queryTask(res.TaskID)
	if err != nil || status != "done" || art == nil || art.URL != "/art.png" {
		t.Fatalf("二轮查询应 done: %s %+v %v", status, art, err)
	}
	// 下载
	data, err := c.download("http://" + strings.TrimPrefix(srv.URL, "http://") + "/art.png")
	if err != nil || len(data) < 100 || sniffImage(data) != "png" {
		t.Fatalf("下载异常: len=%d err=%v", len(data), err)
	}
}

func TestImagegenEditMultipart(t *testing.T) {
	var gotPrompt, gotModel, gotFile string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		gotPrompt = r.FormValue("prompt")
		gotModel = r.FormValue("model")
		f, _, _ := r.FormFile("image")
		if f != nil {
			defer f.Close()
			buf := make([]byte, 512)
			n, _ := f.Read(buf)
			if n > 0 && sniffImage(buf[:n]) != "" {
				gotFile = "ok"
			}
		}
		fmt.Fprint(w, `{"data":[{"url":"http://x/out.png"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	png := t.TempDir() + "/ref.png"
	os.WriteFile(png, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00,
		0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89}, 0o644)

	c := newImagegenClient(srv.URL+"/v1", "k")
	res, err := c.editImage("改成毕加索", png, "auto", "idem-9", "m1")
	if err != nil {
		t.Fatalf("editImage: %v", err)
	}
	if res.URL != "http://x/out.png" || gotPrompt != "改成毕加索" || gotModel != "m1" || gotFile != "ok" {
		t.Fatalf("multipart 字段异常: res=%+v prompt=%q model=%q file=%q", res, gotPrompt, gotModel, gotFile)
	}
}

// TestWorkshopGenerateE2E 生成全链：落单→画手→交稿落库
func TestWorkshopGenerateE2E(t *testing.T) {
	srv, _ := newMockProvider(t, 1)
	s := newTestStore(t)
	dir := t.TempDir()
	p := &hermesPlugin{store: s}
	p.dataDir = dir
	p.metrics = newMetrics()
	p.Config.ImageGenEnabled = true
	p.Config.ImageGenBaseURL = srv.URL + "/v1"
	p.Config.ImageGenAPIKey = "k"
	p.workshop = newWorkshop(p)

	wo := &workorder{Chat: "g@chatroom", Requester: "张三", Kind: woKindGenerate, Prompt: "一只会飞的红薯"}
	if id := s.createWorkorder(wo); id == 0 {
		t.Fatal("落单失败")
	}
	p.workshop.process(wo.ID) // 直接驱动，不起 goroutine

	got := s.getWorkorder(wo.ID)
	if got.Status != woStatusDone || got.FilePath == "" {
		t.Fatalf("应交稿: %+v", got)
	}
	if _, err := os.Stat(got.FilePath); err != nil {
		t.Fatalf("画稿未落盘: %v", err)
	}
	if st := s.workorderStats(); st["today_done"].(int) != 1 {
		t.Fatalf("KPI 未更新: %v", st)
	}
}

// TestWorkshopFail 难产路径：服务商失败→讣告落库
func TestWorkshopFail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"内容政策拦截"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := newTestStore(t)
	p := &hermesPlugin{store: s}
	p.dataDir = t.TempDir()
	p.metrics = newMetrics()
	p.Config.ImageGenEnabled = true
	p.Config.ImageGenBaseURL = srv.URL + "/v1"
	p.Config.ImageGenAPIKey = "k"
	p.workshop = newWorkshop(p)

	wo := &workorder{Chat: "g@chatroom", Requester: "李四", Kind: woKindGenerate, Prompt: "测试"}
	s.createWorkorder(wo)
	p.workshop.process(wo.ID)
	got := s.getWorkorder(wo.ID)
	if got.Status != woStatusFailed || got.Error == "" {
		t.Fatalf("应难产: %+v", got)
	}
}

func TestShrinkAndTruncate(t *testing.T) {
	if got := truncateRunes("一只戴墨镜的橘猫在霓虹灯下吃烤串非常快乐", 10); len([]rune(got)) != 11 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateRunes 异常: %q", got)
	}
	if got := truncateRunes("短", 10); got != "短" {
		t.Fatalf("短文本不应截断: %q", got)
	}
	// 小图不压
	if got := shrinkImage([]byte{0x89, 0x50, 0x4E, 0x47}); len(got) != 4 {
		t.Fatal("小图应原样返回")
	}
}

// ==================== 防复读把关 ====================

func TestNormalizePrompt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"画一张赛博红薯", "赛博红薯"},
		{"再画一张 赛博红薯！！", "赛博红薯"},
		{"帮我画一只戴墨镜的橘猫吧", "一只戴墨镜的橘猫"},
		{"ＰＮＧ风格的 猫！", "png风格的猫"},
		{"赛博红薯。", "赛博红薯"},
		{"画一张赛博红薯好吗", "赛博红薯"},
	}
	for _, c := range cases {
		if got := normalizePrompt(c.in); got != c.want {
			t.Fatalf("normalizePrompt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBigramJaccard(t *testing.T) {
	cases := []struct {
		a, b     string
		min, max float64
	}{
		{"赛博红薯", "赛博红薯", 1, 1},
		{"赛博红薯", "赛博红薯要霓虹灯", 0.40, 0.46},   // 加修饰 → 放行
		{"赛博朋克风橘猫", "赛博朋克风橘狗", 0.68, 0.75}, // 短句(7字)换单字 J=0.5~0.71 → 放行
		// 长句(21字)同长单字替换 J=(21-3)/(21+1)≈0.82 → 拦截（0.8 阈值）
		{"一只戴着墨镜的橘猫站在霓虹闪烁的街头吃烤串", "一只戴着墨镜的狗猫站在霓虹闪烁的街头吃烤串", 0.78, 0.86},
		{"猫", "猫", 1, 1}, // 退化路径
		{"猫", "狗", 0, 0},
	}
	for _, c := range cases {
		got := bigramJaccard(c.a, c.b)
		if got < c.min || got > c.max {
			t.Fatalf("J(%q,%q) = %.3f, 期望 [%.2f,%.2f]", c.a, c.b, got, c.min, c.max)
		}
	}
}

func seedWo(t *testing.T, s *store, chat, requesterID, kind, prompt, size, status string, ageSec int64) *workorder {
	t.Helper()
	wo := &workorder{Chat: chat, Requester: "甲", RequesterID: requesterID, Kind: kind, Prompt: prompt, Size: size}
	if s.createWorkorder(wo) == 0 {
		t.Fatal("落单失败")
	}
	s.mu.Lock()
	_, _ = s.db.Exec("UPDATE workorders SET status=?, created_at=?, done_at=? WHERE id=?",
		status, time.Now().Unix()-ageSec, time.Now().Unix()-ageSec, wo.ID)
	s.mu.Unlock()
	return wo
}

func TestImagegenDedupe(t *testing.T) {
	s := newTestStore(t)
	p := &hermesPlugin{store: s}
	p.dataDir = t.TempDir()

	// chatA：生图精确重复 → 库存直发
	w := seedWo(t, s, "A@chatroom", "wxid_1", woKindGenerate, "画一张赛博红薯！！", "auto", woStatusDone, 60)
	w.Status = woStatusDone // seedWo 只改库；结构体补齐状态再落 FilePath，防 updateWorkorder 写回空状态
	w.FilePath = "/tmp/stock.png"
	w.DoneAt = time.Now().Unix() - 30
	s.updateWorkorder(w)
	if act, m, _ := p.imagegenDedupe("A@chatroom", "wxid_9", woKindGenerate, "赛博红薯", "auto"); act != "reuse" || m.ID != w.ID {
		t.Fatalf("期望 reuse，得 %s %+v", act, m)
	}
	// 主人换了说法带修饰 → 放行（新构图）
	if act, _, _ := p.imagegenDedupe("A@chatroom", "wxid_9", woKindGenerate, "赛博红薯要霓虹灯", "auto"); act != "" {
		t.Fatalf("加修饰应放行，得 %s", act)
	}

	// chatB：在途单 → busy
	seedWo(t, s, "B@chatroom", "wxid_2", woKindGenerate, "赛博红薯", "", woStatusPainting, 30)
	if act, _, _ := p.imagegenDedupe("B@chatroom", "wxid_9", woKindGenerate, "画一张赛博红薯", "auto"); act != "busy" {
		t.Fatalf("期望 busy，得 %s", act)
	}

	// chatC：长句(21字)单字替换 → reject；短句换单字 → 放行
	seedWo(t, s, "C@chatroom", "wxid_3", woKindGenerate, "一只戴着墨镜的橘猫站在霓虹闪烁的街头吃烤串", "auto", woStatusDone, 120)
	if act, _, _ := p.imagegenDedupe("C@chatroom", "wxid_9", woKindGenerate, "一只戴着墨镜的狗猫站在霓虹闪烁的街头吃烤串", "auto"); act != "reject" {
		t.Fatalf("长句高相似应 reject，得 %s", act)
	}
	if act, _, _ := p.imagegenDedupe("C@chatroom", "wxid_9", woKindGenerate, "赛博朋克风橘狗", "auto"); act != "" {
		t.Fatalf("短句换单字应放行，得 %s", act)
	}

	// chatD：难产单不参与查重（重试被允许）
	seedWo(t, s, "D@chatroom", "wxid_4", woKindGenerate, "赛博红薯", "auto", woStatusFailed, 60)
	if act, _, _ := p.imagegenDedupe("D@chatroom", "wxid_9", woKindGenerate, "赛博红薯", "auto"); act != "" {
		t.Fatalf("难产后重试应放行，得 %s", act)
	}

	// chatE：改图单不库存复用（同 core 已交稿）→ 走 L2 相似 → reject
	seedWo(t, s, "E@chatroom", "wxid_5", woKindEdit, "把他改成毕加索风格", "auto", woStatusDone, 60)
	if act, _, why := p.imagegenDedupe("E@chatroom", "wxid_9", woKindEdit, "把他改成毕加索风格！", "auto"); act != "reject" {
		t.Fatalf("改图同句应经 L2 拒绝，得 %s (%s)", act, why)
	}

	// chatF：L1 窗口外（31 分钟）→ 放行
	seedWo(t, s, "F@chatroom", "wxid_6", woKindGenerate, "赛博红薯", "auto", woStatusDone, 31*60)
	if act, _, _ := p.imagegenDedupe("F@chatroom", "wxid_9", woKindGenerate, "赛博红薯", "auto"); act != "" {
		t.Fatalf("窗口外应放行，得 %s", act)
	}

	// chatG：同甲限频（3 单后第 4 单拒绝；他人不受影响）
	for i := 0; i < 3; i++ {
		seedWo(t, s, "G@chatroom", "wxid_7", woKindGenerate, fmt.Sprintf("题材甲第%d号：一只会飞的机械鸽子编号%d", i, i), "auto", woStatusDone, int64(60*(i+1)))
	}
	if act, _, _ := p.imagegenDedupe("G@chatroom", "wxid_7", woKindGenerate, "一只会飞的机械鸽子豪华版", "auto"); act != "reject" {
		t.Fatalf("同甲第4单应拒绝，得 %s", act)
	}
	if act, _, _ := p.imagegenDedupe("G@chatroom", "wxid_999", woKindGenerate, "一只会飞的机械鸽子豪华版", "auto"); act != "" {
		t.Fatalf("其他甲方不应受限，得 %s", act)
	}

	// 库存是全群共享：同句不同甲方 → reuse
	if act, _, _ := p.imagegenDedupe("A@chatroom", "wxid_88", woKindGenerate, "画一张赛博红薯吧", "auto"); act != "reuse" {
		t.Fatalf("库存应全群共享，得 %s", act)
	}
}
