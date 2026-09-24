package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	p := newPlugin()
	p.Config.BridgeToken = "test-token-123"
	p.metrics = newMetrics()
	p.rings = newRingStore(10)
	store := newStore(t.TempDir()+"/state.db", t.TempDir()+"/files")
	t.Cleanup(store.close)
	p.store = store
	p.disp = newDispatcher(&p.Config, nil, nil, p.rings, store, p.metrics)

	// 模拟数据
	p.metrics.addDecision(decisionRec{
		Chat: "12345@chatroom", ChatName: "测试群", IsGroup: true,
		Speaker: "张三", SpeakerID: "wxid_zhang", Outcome: "响应(mention)",
		Trigger: "mention", Profile: "fast", Preview: "你好呀",
	})
	p.metrics.addDecision(decisionRec{
		Chat: "12345@chatroom", ChatName: "测试群", IsGroup: true,
		Speaker: "李四", Outcome: "忽略:群冷却中", Preview: "随便聊聊",
	})
	p.metrics.startRun(&runRec{
		Chat: "12345@chatroom", ChatName: "测试群", Speaker: "张三",
		Trigger: "mention", Profile: "fast", Status: "done",
		ElapsedMS: 2345, PromptTok: 12000, CompTok: 120,
		Reply: "你好！很高兴见到你", Envelope: "[身份信封] chat_type: group", Segments: 1,
	})
	p.rings.add("12345@chatroom", nil) // nil 安全性验证
	store.addUsage(usage{PromptTokens: 100, CompletionTokens: 50}, "fast")

	w := newWebUI(p)
	mux := http.NewServeMux()
	w.mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWebUI(t *testing.T) {
	srv := newTestServer(t)
	client := &http.Client{}

	// 1. UI 页面可达
	r, _ := http.Get(srv.URL + "/ui")
	if r.StatusCode != 200 || !strings.Contains(readAll(t, r), "golem_hermes") {
		t.Fatal("/ui 应返回监控页")
	}
	r.Body.Close()

	// 2. 未登录 401
	r, _ = http.Get(srv.URL + "/api/overview")
	if r.StatusCode != 401 {
		t.Fatal("未登录应 401")
	}
	r.Body.Close()

	// 3. 错误令牌 401
	r, _ = http.Post(srv.URL+"/api/login", "application/json", bytes.NewBufferString(`{"token":"wrong"}`))
	if r.StatusCode != 401 {
		t.Fatal("错误令牌应 401")
	}
	r.Body.Close()

	// 4. 正确令牌登录拿 Cookie
	r, _ = http.Post(srv.URL+"/api/login", "application/json", bytes.NewBufferString(`{"token":"test-token-123"}`))
	if r.StatusCode != 200 {
		t.Fatal("正确令牌应 200")
	}
	var login struct {
		OK bool `json:"ok"`
	}
	_ = json.NewDecoder(r.Body).Decode(&login)
	if !login.OK {
		t.Fatal("登录响应异常")
	}
	cookies := r.Cookies()
	if len(cookies) == 0 {
		t.Fatal("应下发 Cookie")
	}

	get := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		return r.StatusCode, readAll(t, r)
	}

	// 5. 全数据端点
	for _, path := range []string{
		"/api/overview", "/api/runs", "/api/decisions", "/api/chats",
		"/api/usage?days=7", "/api/stickers", "/api/logs", "/api/config",
		"/api/context?key=12345@chatroom",
	} {
		code, body := get(path)
		if code != 200 {
			t.Fatalf("%s 应 200，实际 %d", path, code)
		}
		if !strings.Contains(body, `"ok":true`) {
			t.Fatalf("%s 响应应 ok:true", path)
		}
	}

	// 6. 数据正确性抽查
	_, body := get("/api/decisions")
	if !strings.Contains(body, "响应(mention)") || !strings.Contains(body, "群冷却中") {
		t.Fatal("决策流水数据缺失")
	}
	_, body = get("/api/runs")
	if !strings.Contains(body, "你好！很高兴见到你") {
		t.Fatal("运行记录回复缺失")
	}
	_, body = get("/api/config")
	if strings.Contains(body, "test-token-123") {
		t.Fatal("bridge_token 必须打码")
	}
	_, body = get("/api/overview")
	var ov struct {
		Result struct {
			Inflight int `json:"inflight"`
			Usage    struct {
				Prompt int `json:"prompt"`
			} `json:"usage_today"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &ov); err != nil {
		t.Fatal("overview JSON 解析失败: " + err.Error())
	}
	if ov.Result.Usage.Prompt != 100 {
		t.Fatalf("今日用量应为 100，实际 %d", ov.Result.Usage.Prompt)
	}

	// 7. 写操作
	req, _ := http.NewRequest("POST", srv.URL+"/api/action/retag", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("retag 应 200")
	}

	// 8. 登出后失效
	req, _ = http.NewRequest("POST", srv.URL+"/api/logout", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client.Do(req)
	code, _ := get("/api/overview")
	if code != 401 {
		t.Fatal("登出后应 401")
	}
}

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r.Body)
	return buf.String()
}
