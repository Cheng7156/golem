package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// 可观测性数据层：决策流水 / 运行流水 / 会话聚合 / 通道健康 / 日志环
// 全部进程内存态，有界，随插件生灭
// ============================================================

const (
	maxDecisions = 500
	maxRuns      = 500
	maxLogLines  = 500
)

// decisionRec 单条入站消息的触发决策
type decisionRec struct {
	Time      time.Time `json:"time"`
	Chat      string    `json:"chat"`
	ChatName  string    `json:"chat_name"`
	IsGroup   bool      `json:"is_group"`
	Speaker   string    `json:"speaker"`
	SpeakerID string    `json:"speaker_id"`
	IsOwner   bool      `json:"is_owner"`
	Outcome   string    `json:"outcome"` // 响应(xxx) | 忽略:xxx
	Trigger   string    `json:"trigger"`
	Profile   string    `json:"profile"`
	Preview   string    `json:"preview"`
}

// runRec 一次 Agent 运行
type runRec struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Chat      string    `json:"chat"`
	ChatName  string    `json:"chat_name"`
	Speaker   string    `json:"speaker"`
	Trigger   string    `json:"trigger"` // mention|quote|whitelist|ambient|owner-bypass|private
	Profile   string    `json:"profile"`
	Status    string    `json:"status"` // running|done|interrupted|timeout|error
	ElapsedMS int64     `json:"elapsed_ms"`
	PromptTok int       `json:"prompt_tok"`
	CompTok   int       `json:"comp_tok"`
	Tools     []string  `json:"tools"`
	ReplyLen  int       `json:"reply_len"`
	Reply     string    `json:"reply"`
	Envelope  string    `json:"envelope"`
	Segments  int       `json:"segments"`
	Sticker   string    `json:"sticker"`
	Error     string    `json:"error"`
}

// chatAgg 按聊天聚合
type chatAgg struct {
	Receiver   string    `json:"receiver"`
	Name       string    `json:"name"`
	IsGroup    bool      `json:"is_group"`
	Msgs       int       `json:"msgs"`
	Triggers   int       `json:"triggers"`
	Ambient    int       `json:"ambient"`
	LastActive time.Time `json:"last_active"`
}

// profileHealth 通道探活状态
type profileHealth struct {
	Profile string `json:"profile"`
	OK      bool   `json:"ok"`
	MS      int64  `json:"ms"`
	Last    time.Time `json:"last"`
}

type metrics struct {
	start time.Time
	seq   atomic.Int64

	mu        sync.Mutex
	decisions []decisionRec
	runs      []*runRec
	chats     map[string]*chatAgg
	health    map[string]profileHealth
	logs      []logLine
	runIndex  map[int64]*runRec
}

type logLine struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

func newMetrics() *metrics {
	return &metrics{
		start:    time.Now(),
		chats:    map[string]*chatAgg{},
		health:   map[string]profileHealth{},
		runIndex: map[int64]*runRec{},
		logs:     make([]logLine, 0, maxLogLines),
	}
}

func previewOf(s string, n int) string {
	ra := []rune(strings.TrimSpace(strings.ReplaceAll(s, "\n", "⏎")))
	if len(ra) > n {
		return string(ra[:n]) + "…"
	}
	return string(ra)
}

// ---- 决策 ----

func (m *metrics) addDecision(rec decisionRec) {
	rec.Time = time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions = append(m.decisions, rec)
	if len(m.decisions) > maxDecisions {
		m.decisions = m.decisions[len(m.decisions)-maxDecisions:]
	}
	// 同步累计会话聚合
	ca := m.chats[rec.Chat]
	if ca == nil {
		ca = &chatAgg{Receiver: rec.Chat, Name: rec.ChatName, IsGroup: rec.IsGroup}
		m.chats[rec.Chat] = ca
	}
	ca.Msgs++
	ca.LastActive = rec.Time
	if strings.HasPrefix(rec.Outcome, "响应") {
		ca.Triggers++
		if rec.Trigger == "ambient" || rec.Trigger == "owner-bypass" {
			ca.Ambient++
		}
	}
}

func (m *metrics) decisionsSnapshot(limit int) []decisionRec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.decisions) {
		limit = len(m.decisions)
	}
	out := make([]decisionRec, limit)
	copy(out, m.decisions[len(m.decisions)-limit:])
	// 反转：新的在前
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---- 运行 ----

func (m *metrics) startRun(r *runRec) {
	r.ID = m.seq.Add(1)
	r.Time = time.Now()
	r.Status = "running"
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, r)
	m.runIndex[r.ID] = r
	if len(m.runs) > maxRuns {
		old := m.runs[0]
		m.runs = m.runs[1:]
		delete(m.runIndex, old.ID)
	}
}

func (m *metrics) finishRun(r *runRec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.runIndex[r.ID]; ok {
		*cur = *r
	}
}

func (m *metrics) runsSnapshot(limit int, chat, status string) []*runRec {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*runRec, 0, limit)
	for i := len(m.runs) - 1; i >= 0 && len(out) < limit; i-- {
		r := m.runs[i]
		if chat != "" && r.Chat != chat {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m *metrics) inflightCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.runs {
		if r.Status == "running" {
			n++
		}
	}
	return n
}

// latencyStats 近24h各通道 P50/P95（毫秒）
func (m *metrics) latencyStats() map[string][2]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	byProfile := map[string][]int64{}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, r := range m.runs {
		if r.Status != "done" || r.Time.Before(cutoff) {
			continue
		}
		byProfile[r.Profile] = append(byProfile[r.Profile], r.ElapsedMS)
	}
	out := map[string][2]int64{}
	for p, arr := range byProfile {
		sort.Slice(arr, func(i, j int) bool { return arr[i] < arr[j] })
		pick := func(q float64) int64 {
			idx := int(float64(len(arr)-1) * q)
			return arr[idx]
		}
		out[p] = [2]int64{pick(0.5), pick(0.95)}
	}
	return out
}

// ---- 会话聚合 ----

func (m *metrics) chatsSnapshot() []chatAgg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]chatAgg, 0, len(m.chats))
	for _, ca := range m.chats {
		out = append(out, *ca)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive.After(out[j].LastActive) })
	return out
}

// ---- 健康 ----

func (m *metrics) setHealth(profile string, ok bool, ms int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health[profile] = profileHealth{Profile: profile, OK: ok, MS: ms, Last: time.Now()}
}

func (m *metrics) healthSnapshot() []profileHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]profileHealth, 0, len(m.health))
	for _, h := range m.health {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// 健康探测循环：15s 周期探测基础端口与三个 profile
func (m *metrics) healthLoop(p *hermesPlugin) {
	probes := []string{"base", p.Config.FastProfile, p.Config.FullProfile, p.Config.VisionProfile}
	for {
		for _, name := range probes {
			if name == "" {
				continue
			}
			if p.eng == nil {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			t0 := time.Now()
			err := p.eng.Health(ctx)
			cancel()
			p.metrics.setHealth(name, err == nil, time.Since(t0).Milliseconds())
		}
		time.Sleep(15 * time.Second)
	}
}

// ---- 日志环 ----

func (m *metrics) addLog(level, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, logLine{Time: time.Now(), Level: level, Msg: msg})
	if len(m.logs) > maxLogLines {
		m.logs = m.logs[len(m.logs)-maxLogLines:]
	}
}

func (m *metrics) logsSnapshot(limit int, level string) []logLine {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]logLine, 0, limit)
	for i := len(m.logs) - 1; i >= 0 && len(out) < limit; i-- {
		if level == "" || strings.EqualFold(m.logs[i].Level, level) {
			out = append(out, m.logs[i])
		}
	}
	return out
}

// telemetryHandler slog 分流：既走原 handler，又进环形缓冲
type telemetryHandler struct {
	inner slog.Handler
	m     *metrics
}

func (h *telemetryHandler) Enabled(ctx context.Context, l slog.Level) bool { return true }
func (h *telemetryHandler) Handle(_ context.Context, r slog.Record) error {
	h.m.addLog(r.Level.String(), r.Message)
	return h.inner.Handle(context.Background(), r)
}
func (h *telemetryHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *telemetryHandler) WithGroup(name string) slog.Handler       { return h }

// decisionStats 今日决策分布
func (m *metrics) decisionStats() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	today := time.Now().Format("2006-01-02")
	for _, d := range m.decisions {
		if d.Time.Format("2006-01-02") != today {
			continue
		}
		out[d.Outcome]++
	}
	return out
}
