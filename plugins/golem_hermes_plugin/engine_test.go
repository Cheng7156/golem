package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEngineToolProgressEvents 网关 SSE 的 event: hermes.tool.progress 行必须转成
// evTool 事件——这是调度器流式播报"叙述在工具边界冲刷"的信号源（v0.12.2 曾因
// 忽略 event: 行导致 evTool 永不触发、顺序颠倒）
func TestEngineToolProgressEvents(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"收到，先去搜。\"}}]}\n" +
		"\n" +
		"event: hermes.tool.progress\n" +
		"data: {\"tool\": \"web_search\", \"status\": \"running\"}\n" +
		"\n" +
		"event: hermes.tool.progress\n" +
		"data: {\"tool\": \"web_search\", \"status\": \"completed\"}\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"搜到了。\"}}]}\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n" +
		"\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	cfg := &Config{BaseURL: srv.URL, RunTimeoutSec: 10}
	eng := newEngine(cfg)
	var events []runEvent
	res, err := eng.Run(context.Background(), runRequest{Prompt: "hi", SessionKey: "t"}, func(ev runEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Text != "收到，先去搜。搜到了。" {
		t.Fatalf("全文拼接不对: %q", res.Text)
	}
	var tools []string
	var deltaText strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case evTool:
			tools = append(tools, ev.Tool)
		case evDelta:
			deltaText.WriteString(ev.Text)
		}
	}
	if len(tools) != 2 || tools[0] != "web_search" || tools[1] != "web_search" {
		t.Fatalf("应收到 2 次 web_search 工具事件, got %v", tools)
	}
	if deltaText.String() != "收到，先去搜。搜到了。" {
		t.Fatalf("delta 文本不对: %q", deltaText.String())
	}
	if res.Usage.PromptTokens != 10 {
		t.Fatalf("usage 应正常解析, got %+v", res.Usage)
	}
}
