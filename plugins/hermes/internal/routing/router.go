package routing

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

type Decision struct {
	Route    domain.Route
	Lane     domain.Lane
	Priority int
	Deadline time.Time
	Reason   string
}

type Router interface {
	Route(context.Context, domain.InboxEvent, domain.InboundMessage) (Decision, error)
}

type SocialDecider interface {
	Decide(context.Context, domain.InboxEvent, domain.InboundMessage) (domain.Route, string, error)
}

type RulesRouter struct {
	config func() *config.Snapshot
	social SocialDecider
	now    func() time.Time
}

func NewRulesRouter(snapshot func() *config.Snapshot, social SocialDecider) (*RulesRouter, error) {
	if snapshot == nil {
		return nil, errors.New("routing config snapshot 不能为空")
	}
	return &RulesRouter{config: snapshot, social: social, now: time.Now}, nil
}

func (r *RulesRouter) Route(
	ctx context.Context,
	event domain.InboxEvent,
	message domain.InboundMessage,
) (Decision, error) {
	cfg := r.config()
	if cfg == nil {
		return Decision{}, errors.New("routing config 不可用")
	}
	now := r.now()
	if message.Explicit() {
		if isControlCommand(message.Text) {
			return Decision{
				Route:    domain.RouteControl,
				Lane:     domain.LaneControl,
				Priority: 1000,
				Deadline: now.Add(10 * time.Second),
				Reason:   "local control command",
			}, nil
		}
		if likelyLongTask(message.Text) {
			return Decision{
				Route:    domain.RouteJob,
				Lane:     domain.LaneJob,
				Priority: 90,
				Deadline: agentDeadline(cfg, now, 10*time.Minute),
				Reason:   "明确消息需要工具或长时间处理",
			}, nil
		}
		return Decision{
			Route:    domain.RouteChat,
			Lane:     domain.LaneInteractive,
			Priority: 100,
			Deadline: agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
			Reason:   "私聊、@ 或引用",
		}, nil
	}

	switch cfg.Routing.SocialMode {
	case "observe", "rules":
		return Decision{Route: domain.RouteObserve, Reason: "普通群聊由本地模式保持观察"}, nil
	case "agent":
		return Decision{
			Route:    domain.RouteChat,
			Lane:     domain.LaneInteractive,
			Priority: 50,
			Deadline: agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
			Reason:   "普通群聊交给 Hermes 结合共享上下文自主决定是否参与",
		}, nil
	case "hybrid":
		if r.social == nil || !sampled(event.ID, cfg.Routing.SampleRate) {
			return Decision{Route: domain.RouteObserve, Reason: "Social Router 未调用或未命中采样"}, nil
		}
		timeout := time.Duration(cfg.Routing.DecisionTimeoutMilliseconds) * time.Millisecond
		decisionCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		route, reason, err := r.social.Decide(decisionCtx, event, message)
		if err != nil {
			return Decision{Route: domain.RouteObserve, Reason: "Social Router 失败，降级观察"}, nil
		}
		switch route {
		case domain.RouteChat:
			return Decision{
				Route:    route,
				Lane:     domain.LaneInteractive,
				Priority: 50,
				Deadline: agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
				Reason:   reason,
			}, nil
		case domain.RouteJob:
			return Decision{
				Route:    route,
				Lane:     domain.LaneJob,
				Priority: 40,
				Deadline: agentDeadline(cfg, now, 10*time.Minute),
				Reason:   reason,
			}, nil
		default:
			return Decision{Route: domain.RouteObserve, Reason: reason}, nil
		}
	default:
		return Decision{Route: domain.RouteObserve, Reason: "未知路由模式"}, nil
	}
}

func agentDeadline(cfg *config.Snapshot, now time.Time, timeout time.Duration) time.Time {
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Agent.Mode), "relay") {
		return time.Time{}
	}
	return now.Add(timeout)
}

func isControlCommand(value string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(fields) == 0 {
		return false
	}
	command := strings.SplitN(fields[0], "@", 2)[0]
	return command == "/stop" || command == "/cancel"
}

func likelyLongTask(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, keyword := range []string{
		"搜索", "查一下", "分析", "总结", "整理", "生成", "报告", "图片",
		"识别", "渲染", "下载", "文件", "视频", "表格", "对比", "翻译",
	} {
		if strings.Contains(value, keyword) {
			return true
		}
	}
	return false
}

func sampled(id string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	hash := sha256.Sum256([]byte(id))
	value := binary.BigEndian.Uint64(hash[:8])
	return float64(value)/float64(^uint64(0)) < rate
}
