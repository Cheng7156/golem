package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	evDelta = "delta"
	evTool  = "tool"
)

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u usage) total() int { return u.PromptTokens + u.CompletionTokens }

// runRequest 一次 Agent 请求
type runRequest struct {
	SessionKey string // X-Hermes-Session-Key，会话续接与路由
	Profile    string // 多路 profile（fast/full），空=默认
	Prompt     string
	Image      []byte // 可选图片（vision）
	ImageMime  string
}

type runEvent struct {
	Type string // delta | tool
	Text string
	Tool string
}

type engineResult struct {
	Text      string
	SessionID string
	Usage     usage
}

// Engine Agent 引擎抽象：HTTP(SSE) 是首个实现，后续可替换
type Engine interface {
	Run(ctx context.Context, req runRequest, onEvent func(runEvent)) (engineResult, error)
	Complete(ctx context.Context, req runRequest) (engineResult, error)
	Health(ctx context.Context) error
}

// httpEngine hermes gateway API server 的 OpenAI 兼容实现
type httpEngine struct {
	cfg *Config
	hc  *http.Client

	mu       sync.Mutex
	sessions map[string]string // sessionKey -> hermes session id
}

func newEngine(cfg *Config) *httpEngine {
	return &httpEngine{
		cfg:      cfg,
		hc:       &http.Client{},
		sessions: map[string]string{},
	}
}

func (e *httpEngine) endpoint(profile, path string) string {
	base := strings.TrimRight(e.cfg.BaseURL, "/")
	if profile != "" {
		base += "/p/" + profile
	}
	return base + path
}

// ---- OpenAI 兼容 wire 类型 ----

type imageURL struct {
	URL string `json:"url"`
}

type chatContentPart struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatRequest struct {
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message *struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
	// hermes 自定义事件负载（尽力解析，不强制）
	Tool    any    `json:"tool"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Content string `json:"content"`
}

func (e *httpEngine) buildBody(req runRequest, stream bool) ([]byte, error) {
	msg := chatMessage{Role: "user"}
	if len(req.Image) > 0 {
		mime := req.ImageMime
		if mime == "" {
			mime = "image/jpeg"
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(req.Image)
		msg.Content = []chatContentPart{
			{Type: "text", Text: req.Prompt},
			{Type: "image_url", ImageURL: &imageURL{URL: dataURL}},
		}
	} else {
		msg.Content = req.Prompt
	}
	return json.Marshal(chatRequest{Messages: []chatMessage{msg}, Stream: stream})
}

func (e *httpEngine) newRequest(ctx context.Context, req runRequest, body []byte, stream bool) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint(req.Profile, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	httpReq.Header.Set("X-Hermes-Session-Key", req.SessionKey)
	if sid := e.sessionID(req.SessionKey); sid != "" {
		httpReq.Header.Set("X-Hermes-Session-Id", sid)
	}
	return httpReq, nil
}

func (e *httpEngine) sessionID(key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[key]
}

func (e *httpEngine) setSessionID(key, id string) {
	if key == "" || id == "" {
		return
	}
	e.mu.Lock()
	e.sessions[key] = id
	e.mu.Unlock()
}

// ChatKeyByHermesSession 反查：hermes 会话 ID → 本地会话键（工具桥精确绑定用）
func (e *httpEngine) ChatKeyByHermesSession(sid string) string {
	if sid == "" {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, id := range e.sessions {
		if id == sid {
			return key
		}
	}
	return ""
}

// Run 流式运行一轮；onEvent 可为 nil
func (e *httpEngine) Run(ctx context.Context, req runRequest, onEvent func(runEvent)) (engineResult, error) {
	body, err := e.buildBody(req, true)
	if err != nil {
		return engineResult{}, err
	}
	httpReq, err := e.newRequest(ctx, req, body, true)
	if err != nil {
		return engineResult{}, err
	}
	resp, err := e.hc.Do(httpReq)
	if err != nil {
		return engineResult{}, err
	}
	defer resp.Body.Close()

	var res engineResult
	if sid := resp.Header.Get("X-Hermes-Session-Id"); sid != "" {
		res.SessionID = sid
		e.setSessionID(req.SessionKey, sid)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return res, fmt.Errorf("hermes api %s: %d %s", e.endpoint(req.Profile, ""), resp.StatusCode, strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // 忽略无法解析的心跳/事件行
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return res, fmt.Errorf("hermes error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			res.Usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				res.Text += choice.Delta.Content
				if onEvent != nil {
					onEvent(runEvent{Type: evDelta, Text: choice.Delta.Content})
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("读取流失败: %w", err)
	}
	return res, nil
}

// Complete 非流式一轮（表情打标等系统任务用）
func (e *httpEngine) Complete(ctx context.Context, req runRequest) (engineResult, error) {
	body, err := e.buildBody(req, false)
	if err != nil {
		return engineResult{}, err
	}
	httpReq, err := e.newRequest(ctx, req, body, false)
	if err != nil {
		return engineResult{}, err
	}
	resp, err := e.hc.Do(httpReq)
	if err != nil {
		return engineResult{}, err
	}
	defer resp.Body.Close()

	var res engineResult
	if sid := resp.Header.Get("X-Hermes-Session-Id"); sid != "" {
		e.setSessionID(req.SessionKey, sid)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return res, err
	}
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("hermes api %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out streamChunk
	if err := json.Unmarshal(raw, &out); err != nil {
		return res, fmt.Errorf("解析响应失败: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return res, fmt.Errorf("hermes error: %s", out.Error.Message)
	}
	if out.Usage != nil {
		res.Usage = *out.Usage
	}
	for _, choice := range out.Choices {
		if choice.Message != nil && choice.Message.Content != "" {
			res.Text += choice.Message.Content
		}
	}
	return res, nil
}

// Health 探测 gateway 主端口
func (e *httpEngine) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(e.cfg.BaseURL, "/") + "/health"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if e.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.hc.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("gateway health %d", resp.StatusCode)
	}
	return nil
}
