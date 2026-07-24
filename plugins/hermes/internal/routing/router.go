package routing

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

type Decision struct {
	Route       domain.Route
	Disposition SocialDisposition
	Lane        domain.Lane
	Priority    int
	Deadline    time.Time
	Reason      string
}

type SocialDisposition string

const (
	DispositionIgnore  SocialDisposition = "ignore"
	DispositionObserve SocialDisposition = "observe"
	DispositionRespond SocialDisposition = "respond"
)

type SocialDecision struct {
	Disposition SocialDisposition
	Route       domain.Route
	Reason      string
	Confidence  float64
}

type Router interface {
	Route(context.Context, domain.InboxEvent, domain.InboundMessage) (Decision, error)
}

type SocialDecider interface {
	Decide(context.Context, domain.InboxEvent, domain.InboundMessage) (SocialDecision, error)
}

type AmbientContextReader interface {
	ListRecentInboundContext(context.Context, string, int64, int) ([]domain.ContextMessage, error)
	HasNewerInboundFromSpeaker(context.Context, string, int64, string, time.Time) (bool, error)
}

type RulesRouter struct {
	config  func() *config.Snapshot
	social  SocialDecider
	context AmbientContextReader
	now     func() time.Time
	mu      sync.Mutex
	states  map[string]*ambientState
}

type ambientState struct {
	lastHash string
	lastSeen time.Time
}

func NewRulesRouter(
	snapshot func() *config.Snapshot,
	social SocialDecider,
	contextReaders ...AmbientContextReader,
) (*RulesRouter, error) {
	if snapshot == nil {
		return nil, errors.New("routing config snapshot 不能为空")
	}
	var contextReader AmbientContextReader
	if len(contextReaders) > 0 {
		contextReader = contextReaders[0]
	}
	return &RulesRouter{
		config: snapshot, social: social, context: contextReader, now: time.Now,
		states: map[string]*ambientState{},
	}, nil
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
	if message.VisualMediaOnly {
		return Decision{
			Route:       domain.RouteObserve,
			Disposition: DispositionObserve,
			Reason:      "独立图片或表情先保存为上下文，由 Agent 工具按需查询",
		}, nil
	}
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
		return Decision{
			Route:       domain.RouteChat,
			Disposition: DispositionRespond,
			Lane:        domain.LaneInteractive,
			Priority:    100,
			Deadline:    agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
			Reason:      "私聊、@ 或引用",
		}, nil
	}
	if cfg.Routing.SocialMode == "agent" || cfg.Routing.SocialMode == "hybrid" {
		if reason, observe := r.coalesceAmbient(ctx, event, message, cfg, now); observe {
			return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: reason}, nil
		}
	}

	switch cfg.Routing.SocialMode {
	case "observe", "rules":
		return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: "普通群聊由本地模式保持观察"}, nil
	case "mentions":
		return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: "mentions 模式仅将私聊、@机器人或引用机器人交给 Hermes"}, nil
	case "agent":
		if !sampled(event.ID, cfg.Routing.SampleRate) {
			return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: "普通群聊未命中 Hermes 采样"}, nil
		}
		return Decision{
			Route:       domain.RouteChat,
			Disposition: DispositionRespond,
			Lane:        domain.LaneInteractive,
			Priority:    50,
			Deadline:    agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
			Reason:      "普通群聊交给 Hermes 结合共享上下文自主决定是否参与",
		}, nil
	case "hybrid":
		if reason, observe := r.fastObserve(ctx, event, message, cfg, now); observe {
			return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: reason}, nil
		}
		if event.Binding.Principal.IsOwner {
			return Decision{
				Route:       domain.RouteChat,
				Disposition: DispositionRespond,
				Lane:        domain.LaneInteractive,
				Priority:    75,
				Deadline:    agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
				Reason:      "主人普通群聊在基础安全过滤后直接交给 Hermes，以保持连续对话",
			}, nil
		}
		if r.social == nil || !sampled(event.ID, cfg.Routing.SampleRate) {
			return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: "Social Router 未调用或未命中采样"}, nil
		}
		timeout := time.Duration(cfg.Routing.DecisionTimeoutMilliseconds) * time.Millisecond
		decisionCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		social, err := r.social.Decide(decisionCtx, event, message)
		if err != nil {
			return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: "Social Router 失败，降级观察"}, nil
		}
		switch social.Route {
		case domain.RouteChat, domain.RouteJob:
			return Decision{
				Route:       domain.RouteChat,
				Disposition: DispositionRespond,
				Lane:        domain.LaneInteractive,
				Priority:    50,
				Deadline:    agentDeadline(cfg, now, time.Duration(cfg.Agent.TimeoutSeconds)*time.Second),
				Reason:      social.Reason,
			}, nil
		default:
			return Decision{Route: domain.RouteObserve, Disposition: social.Disposition, Reason: social.Reason}, nil
		}
	default:
		return Decision{Route: domain.RouteObserve, Disposition: DispositionObserve, Reason: "未知路由模式"}, nil
	}
}

func (r *RulesRouter) fastObserve(
	ctx context.Context,
	event domain.InboxEvent,
	message domain.InboundMessage,
	cfg *config.Snapshot,
	now time.Time,
) (string, bool) {
	if message.MentionedOthers {
		return "消息明确 @ 其他参与者", true
	}
	if !message.OccurredAt.IsZero() && now.Sub(message.OccurredAt) > time.Duration(cfg.Routing.OrdinaryFreshnessSeconds)*time.Second {
		return "普通群消息已过参与时效", true
	}
	if automatedSpeaker(cfg.Routing, event.Binding.Principal, message) {
		return "已配置的自动化发送者默认只进入影子上下文", true
	}
	if automatedBroadcast(message.Text) {
		return "自动化播报或静默元消息只进入影子上下文", true
	}
	if r.duplicateAmbient(event.SessionID, message, now) {
		return "短时间重复群消息", true
	}
	return "", false
}

func (r *RulesRouter) coalesceAmbient(
	ctx context.Context,
	event domain.InboxEvent,
	message domain.InboundMessage,
	cfg *config.Snapshot,
	now time.Time,
) (string, bool) {
	if r.context == nil || event.AcceptSeq <= 0 || strings.TrimSpace(message.SpeakerID) == "" {
		return "", false
	}
	coalesceWindow := time.Duration(cfg.Routing.CoalesceWindowMilliseconds) * time.Millisecond
	until := message.OccurredAt.Add(coalesceWindow)
	if !message.OccurredAt.IsZero() {
		if remaining := until.Sub(now); remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return "分段消息合并等待被取消", true
			case <-timer.C:
			}
		}
	}
	newer, err := r.context.HasNewerInboundFromSpeaker(
		ctx, event.SessionID, event.AcceptSeq, message.SpeakerID, until,
	)
	if err != nil {
		slog.Debug("[hermes] 检查分段消息失败，继续语义决策", "event_id", event.ID, "err", err)
		return "", false
	}
	if newer {
		return "同一发送者存在紧随其后的消息，本条只作为分段上下文", true
	}
	return "", false
}

func automatedSpeaker(
	cfg config.RoutingConfig,
	principal domain.Principal,
	message domain.InboundMessage,
) bool {
	id := strings.TrimSpace(principal.ID)
	if id == "" {
		id = strings.TrimSpace(message.SpeakerID)
	}
	for _, candidate := range cfg.AutomatedSpeakerIDs {
		if id != "" && strings.EqualFold(id, candidate) {
			return true
		}
	}
	name := strings.TrimSpace(principal.Name)
	if name == "" {
		name = strings.TrimSpace(message.SpeakerName)
	}
	for _, candidate := range cfg.AutomatedSpeakerNames {
		if name != "" && strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

func automatedBroadcast(value string) bool {
	text := strings.ToLower(strings.TrimSpace(value))
	if text == "" {
		return true
	}
	for _, marker := range []string{
		"self-improvement review:", "plugin process exited", "仙途奇遇", "获得 ",
		"（没被点到", "(没被点到", "empty response", "not addressed to me",
		"[[golem_hermes_observe_v1]]", "[relay: silent]",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (r *RulesRouter) duplicateAmbient(sessionID string, message domain.InboundMessage, now time.Time) bool {
	value := strings.ToLower(strings.TrimSpace(message.SpeakerID + "\x00" + message.Text))
	if value == "" {
		return false
	}
	hash := sha256.Sum256([]byte(value))
	key := string(hash[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[sessionID]
	if state == nil {
		state = &ambientState{}
		r.states[sessionID] = state
	}
	duplicate := state.lastHash == key && now.Sub(state.lastSeen) <= 2*time.Minute
	state.lastHash, state.lastSeen = key, now
	return duplicate
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
