package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/tool"
)

type Worker struct {
	id            int
	store         storeport.Store
	engine        agent.Engine
	tools         tool.Registry
	media         MediaResolver
	lane          domain.Lane
	config        func() *config.Snapshot
	wake          <-chan struct{}
	outputWake    chan<- struct{}
	pollInterval  time.Duration
	leaseDuration time.Duration
	now           func() time.Time
}

type MediaResolver interface {
	Resolve(context.Context, []domain.InboundMedia) ([]domain.InboundMedia, error)
}

func NewWorker(
	id int,
	store storeport.Store,
	engine agent.Engine,
	tools tool.Registry,
	media MediaResolver,
	lane domain.Lane,
	snapshot func() *config.Snapshot,
	wake <-chan struct{},
	outputWake chan<- struct{},
) (*Worker, error) {
	if store == nil || engine == nil || tools == nil || snapshot == nil {
		return nil, errors.New("execution worker requires store, engine, tools, and config")
	}
	return &Worker{
		id:            id,
		store:         store,
		engine:        engine,
		tools:         tools,
		media:         media,
		lane:          lane,
		config:        snapshot,
		wake:          wake,
		outputWake:    outputWake,
		pollInterval:  50 * time.Millisecond,
		leaseDuration: 45 * time.Second,
		now:           time.Now,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		run, err := w.store.LeaseNextRun(ctx, w.lane, w.now(), w.leaseDuration)
		if errors.Is(err, storeport.ErrNotFound) {
			if err := w.wait(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := w.execute(ctx, run); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("[hermes] run failed",
				"worker", w.id,
				"run_id", run.ID,
				"lane", run.Lane,
				"err", err,
			)
		}
	}
}

func (w *Worker) wait(ctx context.Context) error {
	timer := time.NewTimer(w.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (w *Worker) execute(parent context.Context, run domain.Run) error {
	if err := w.store.MarkRunRunning(parent, run.ID, run.LeaseToken); err != nil {
		return err
	}
	turn, err := w.store.GetTurn(parent, run.TurnID)
	if err != nil {
		return w.finishFailure(parent, run, err)
	}
	inbox, err := w.store.GetInbox(parent, turn.EventID)
	if err != nil {
		return w.finishFailure(parent, run, err)
	}
	var incoming domain.InboundMessage
	if err := json.Unmarshal(inbox.Payload, &incoming); err != nil {
		return w.finishFailure(parent, run, fmt.Errorf("decode inbound message: %w", err))
	}
	cfg := w.config()
	if cfg == nil {
		return w.finishFailure(parent, run, errors.New("agent configuration is unavailable"))
	}
	runCtx, cancel, deadline := executionContext(parent, cfg, run, w.now())
	defer cancel()
	if err := w.waitContextBarrier(runCtx, run); err != nil {
		return w.finishFailure(parent, run, fmt.Errorf("wait observation context barrier: %w", err))
	}
	media := append([]domain.InboundMedia(nil), incoming.Media...)
	if w.media != nil {
		media, err = w.media.Resolve(runCtx, media)
		if err != nil {
			return w.finishFailure(parent, run, fmt.Errorf("resolve inbound media: %w", err))
		}
	}
	scope := toolScope(run, inbox.Binding.Principal)
	var contextMessages []domain.ContextMessage
	if cfg.Context.Mode == "legacy_shadow" {
		contextMessages = w.shadowContext(runCtx, inbox, cfg)
	}
	stream, err := w.engine.Start(runCtx, agent.RunRequest{
		RunID:                run.ID,
		SessionID:            run.SessionID,
		Principal:            inbox.Binding.Principal,
		Lane:                 run.Lane,
		BaseSessionVersion:   turn.BaseSessionVersion,
		Input:                formatAgentInputWithContext(cfg.Agent.Mode, incoming, inbox.Binding.Principal, contextMessages),
		SessionNamespace:     cfg.Agent.RelaySessionNamespace,
		SystemPrompt:         cfg.Agent.SystemPrompt,
		Model:                cfg.Agent.Model,
		ToolSpecs:            w.tools.Specs(runCtx, scope),
		Deadline:             deadline,
		Checkpoint:           append(json.RawMessage(nil), run.Checkpoint...),
		Revision:             run.Revision,
		ConversationID:       run.ConversationID,
		CurrentObservationID: run.CurrentObservationID,
		CurrentPayloadHash:   run.CurrentPayloadHash,
		RequiredContextSeq:   run.RequiredContextSeq,
		TriggerKind:          run.TriggerKind,
		InvocationID:         run.InvocationID,
		VerifiedActor:        verifiedActor(inbox.Binding.Principal),
		Addressing:           observationAddressing(incoming),
		ChatType:             chatType(incoming),
		ChatName:             incoming.RoomName,
		MessageID:            inbox.ID,
		RequireVisibleReply:  incoming.Explicit(),
		Media:                media,
	})
	if err != nil {
		return w.finishFailure(parent, run, err)
	}
	defer stream.Close()

	var drafts []domain.OutboxDraft
	var lastSequence uint64
	completed := false
	var durableResult *domain.RelayRunResult
	for {
		event, recvErr := stream.Recv(runCtx)
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) && completed {
				break
			}
			if parent.Err() != nil {
				return parent.Err()
			}
			if cancelled, cancelErr := w.cancelIfRequested(parent, run); cancelled {
				return cancelErr
			} else if cancelErr != nil {
				return cancelErr
			}
			return w.finishFailure(parent, run, recvErr)
		}
		if err := validateEvent(run.ID, lastSequence, event); err != nil {
			return w.finishFailure(parent, run, err)
		}
		lastSequence = event.Sequence
		switch event.Kind {
		case agent.EventReplyProposed, agent.EventEffectProposed, agent.EventProgress:
			draft, proposalErr := outputDraft(run, inbox.Binding.ReceiverID, event)
			if proposalErr != nil {
				return w.finishFailure(parent, run, proposalErr)
			}
			drafts = append(drafts, draft)
		case agent.EventCheckpoint:
			if err := w.store.SaveRunCheckpoint(parent, run.ID, run.LeaseToken, event.Checkpoint); err != nil {
				return w.finishFailure(parent, run, err)
			}
		case agent.EventToolCallRequested:
			if event.ToolCall == nil {
				return w.finishFailure(parent, run, errors.New("agent requested a nil tool call"))
			}
			call := *event.ToolCall
			if call.RunID == "" {
				call.RunID = run.ID
			}
			if call.RunID != run.ID {
				return w.finishFailure(parent, run, errors.New("tool call run_id mismatch"))
			}
			result, invokeErr := w.tools.Invoke(runCtx, scope, call)
			if invokeErr != nil {
				result.InvocationID = call.InvocationID
				result.Error = invokeErr.Error()
			}
			if err := stream.Send(runCtx, agent.Command{
				Kind:       agent.CommandToolResult,
				RunID:      run.ID,
				ToolResult: &result,
			}); err != nil {
				return w.finishFailure(parent, run, err)
			}
		case agent.EventRunFailed:
			if event.Err == nil {
				event.Err = errors.New("agent run failed")
			}
			if cancelled, cancelErr := w.cancelIfRequested(parent, run); cancelled {
				return cancelErr
			} else if cancelErr != nil {
				return cancelErr
			}
			return w.finishFailure(parent, run, event.Err)
		case agent.EventRunCompleted:
			if event.ProposalID != "" {
				durableResult = &domain.RelayRunResult{ProposalID: event.ProposalID,
					InvocationID: event.InvocationID, RunID: run.ID, ResultKind: event.ResultKind,
					ResultHash: event.ResultHash}
			}
			completed = true
		}
		if completed {
			break
		}
	}

	if cancelled, cancelErr := w.cancelIfRequested(parent, run); cancelled {
		return cancelErr
	} else if cancelErr != nil {
		return cancelErr
	}
	if guarded, reason := guardAmbientDrafts(drafts, incoming, inbox.Binding.Principal, cfg.Routing); reason != "" {
		slog.Warn("[hermes] 回复守卫抑制了高风险 ambient 输出",
			"run_id", run.ID,
			"session_id", run.SessionID,
			"speaker", incoming.SpeakerName,
			"reason", reason,
		)
		drafts = guarded
	}
	if durableResult != nil {
		_, err = w.store.CommitRelayRunResult(parent, run.ID, run.LeaseToken, *durableResult, drafts)
		sendErr := stream.Send(parent, agent.Command{Kind: agent.CommandProposalResult, RunID: run.ID,
			ProposalID: durableResult.ProposalID, Err: err})
		if err != nil {
			return err
		}
		if sendErr != nil {
			return sendErr
		}
	} else if _, err := w.store.CommitRunSuccess(parent, run.ID, run.LeaseToken, drafts); err != nil {
		return err
	}
	signal(w.outputWake)
	return nil
}

type observationBarrierStore interface {
	ObservationContextReady(context.Context, string, int64) (bool, error)
}

func (w *Worker) waitContextBarrier(ctx context.Context, run domain.Run) error {
	observer, ok := w.engine.(agent.ObservationGateway)
	cfg := w.config()
	if cfg == nil || cfg.Context.Mode != "full" || !ok || !observer.SupportsObservationV2() || run.RequiredContextSeq <= 0 {
		return nil
	}
	store, ok := w.store.(observationBarrierStore)
	if !ok {
		return errors.New("store does not support observation context barrier")
	}
	for {
		ready, err := store.ObservationContextReady(ctx, run.ConversationID, run.RequiredContextSeq)
		if err != nil || ready {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func verifiedActor(principal domain.Principal) domain.VerifiedActor {
	role := "participant_not_owner"
	if principal.IsOwner {
		role = "owner_of_this_agent"
	}
	return domain.VerifiedActor{ActorID: principal.ID, DisplayName: principal.Name, Role: role,
		ActorKind: "unknown", VerifiedBy: "golem_wechat_protocol"}
}

func observationAddressing(message domain.InboundMessage) domain.Addressing {
	return domain.Addressing{Self: message.Mentioned, Others: message.MentionedOthers,
		QuotedSelf: message.Quoted, MentionTargetIDs: []string{}}
}

type inboundContextReader interface {
	ListRecentInboundContext(context.Context, string, int64, int) ([]domain.ContextMessage, error)
}

func (w *Worker) shadowContext(
	ctx context.Context,
	inbox domain.InboxEvent,
	cfg *config.Snapshot,
) []domain.ContextMessage {
	if cfg == nil || inbox.AcceptSeq <= 0 {
		return nil
	}
	reader, ok := w.store.(inboundContextReader)
	if !ok {
		return nil
	}
	limit := max(4, cfg.Routing.DecisionContextMessages*3)
	values, err := reader.ListRecentInboundContext(ctx, inbox.SessionID, inbox.AcceptSeq, limit)
	if err != nil {
		slog.Debug("[hermes] 读取影子群聊上下文失败", "event_id", inbox.ID, "err", err)
		return nil
	}
	start := 0
	for index, value := range values {
		if value.Route != "" && value.Route != domain.RouteObserve {
			start = index + 1
		}
	}
	values = values[start:]
	if len(values) > cfg.Routing.DecisionContextMessages {
		values = values[len(values)-cfg.Routing.DecisionContextMessages:]
	}
	result := make([]domain.ContextMessage, 0, len(values))
	for _, value := range values {
		if value.Route == domain.RouteObserve {
			result = append(result, value)
		}
	}
	return result
}

func guardAmbientDrafts(
	drafts []domain.OutboxDraft,
	message domain.InboundMessage,
	principal domain.Principal,
	cfg config.RoutingConfig,
) ([]domain.OutboxDraft, string) {
	if !message.IsChatroom || message.Explicit() || len(drafts) == 0 {
		return drafts, ""
	}
	if message.MentionedOthers {
		return nil, "当前消息明确发给其他参与者"
	}
	if configuredAutomatedSpeaker(cfg, principal, message) {
		return nil, "自动化发送者的 ambient 消息不得产生可见回复"
	}
	if standaloneInboundMedia(message) {
		return nil, "未点名的独立图片或表情不得产生可见回复"
	}
	if !principal.IsOwner {
		for _, content := range textDraftContents(drafts) {
			lower := strings.ToLower(content)
			if strings.Contains(content, "主人") || strings.Contains(lower, "my owner") || strings.Contains(lower, "my master") {
				return nil, "非主人 ambient 输入触发了主人关系认领风险"
			}
		}
	}
	return drafts, ""
}

func configuredAutomatedSpeaker(
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

func standaloneInboundMedia(message domain.InboundMessage) bool {
	if len(message.Media) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(message.Text)) {
	case "", "[sticker]", "[emoji]", "[image]", "[图片]", "[表情包]":
		return true
	default:
		return false
	}
}

func textDraftContents(drafts []domain.OutboxDraft) []string {
	var result []string
	for _, draft := range drafts {
		if draft.Kind != "text" {
			continue
		}
		var output domain.TextOutput
		if json.Unmarshal(draft.Payload, &output) == nil {
			result = append(result, output.Content)
		}
	}
	return result
}

func (w *Worker) cancelIfRequested(ctx context.Context, run domain.Run) (bool, error) {
	current, err := w.store.GetRun(ctx, run.ID)
	if err != nil {
		return false, err
	}
	if current.State != domain.RunCancelRequested {
		return false, nil
	}
	if err := w.store.MarkRunCancelled(ctx, run.ID, run.LeaseToken); err != nil {
		return true, err
	}
	return true, context.Canceled
}

func validateEvent(runID string, previous uint64, event agent.Event) error {
	if event.RunID != "" && event.RunID != runID {
		return errors.New("agent event run_id mismatch")
	}
	if event.Sequence == 0 || event.Sequence <= previous {
		return fmt.Errorf("agent event sequence %d is not after %d", event.Sequence, previous)
	}
	return nil
}

func outputDraft(run domain.Run, receiverID string, event agent.Event) (domain.OutboxDraft, error) {
	proposal := event.Proposal
	if proposal == nil && strings.TrimSpace(event.Text) != "" {
		value, err := agent.NewTextProposal(event.Text)
		if err != nil {
			return domain.OutboxDraft{}, err
		}
		proposal = &value
	}
	if proposal == nil {
		return domain.OutboxDraft{}, errors.New("agent output event has no proposal")
	}
	if err := proposal.Validate(); err != nil {
		return domain.OutboxDraft{}, err
	}
	if err := validateOutputPayload(*proposal); err != nil {
		return domain.OutboxDraft{}, err
	}
	return domain.OutboxDraft{
		SessionID:  run.SessionID,
		ReceiverID: receiverID,
		Kind:       proposal.Kind,
		Payload:    append(json.RawMessage(nil), proposal.Payload...),
	}, nil
}

func validateOutputPayload(proposal agent.OutputProposal) error {
	switch proposal.Kind {
	case "text":
		var output domain.TextOutput
		if err := json.Unmarshal(proposal.Payload, &output); err != nil {
			return err
		}
		if strings.TrimSpace(output.Content) == "" {
			return errors.New("text output is empty")
		}
	case "image":
		var output domain.ImageOutput
		if err := json.Unmarshal(proposal.Payload, &output); err != nil {
			return err
		}
		if strings.TrimSpace(output.URL) == "" && len(output.Data) == 0 {
			return errors.New("image output requires url or data")
		}
	case "emoji":
		var output domain.EmojiOutput
		if err := json.Unmarshal(proposal.Payload, &output); err != nil {
			return err
		}
		if strings.TrimSpace(output.URL) == "" && len(output.Data) == 0 {
			return errors.New("emoji output requires url or data")
		}
	case "video":
		var output domain.VideoOutput
		if err := json.Unmarshal(proposal.Payload, &output); err != nil {
			return err
		}
		if strings.TrimSpace(output.ObjectID) == "" || strings.TrimSpace(output.ThumbObjectID) == "" {
			return errors.New("video output requires object_id and thumb_object_id")
		}
		if output.Duration == 0 {
			return errors.New("video output requires duration")
		}
	default:
		return fmt.Errorf("unsupported output proposal kind %q", proposal.Kind)
	}
	return nil
}

func toolScope(run domain.Run, principal domain.Principal) tool.Scope {
	capabilities := []tool.Capability{
		"history.read.current_session",
		"image.read.current_session",
		"memory.read.current_session",
		"output.propose.current_session",
	}
	if principal.IsOwner {
		capabilities = append(capabilities,
			"memory.write.current_session",
			"policy.write.owner_only",
		)
	}
	return tool.Scope{
		RunID:        run.ID,
		SessionID:    run.SessionID,
		Principal:    principal,
		Lane:         run.Lane,
		Capabilities: capabilities,
	}
}

func chatType(message domain.InboundMessage) string {
	if message.IsChatroom {
		return "group"
	}
	return "dm"
}

func (w *Worker) finishFailure(ctx context.Context, run domain.Run, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cancelled, cancelErr := w.cancelIfRequested(ctx, run); cancelled {
		return cancelErr
	} else if cancelErr != nil {
		return errors.Join(cause, cancelErr)
	}
	retryable := shouldRetryRun(w.config(), run, w.now())
	if retryable {
		delay := time.Duration(1<<min(run.Attempt, 6)) * time.Second
		nextAttempt := w.now().Add(delay)
		if err := w.store.FailRun(
			ctx,
			run.ID,
			run.LeaseToken,
			cause.Error(),
			true,
			nextAttempt,
		); err != nil {
			return errors.Join(cause, err)
		}
		slog.Warn("[hermes] Run 已保留并等待重试",
			"run_id", run.ID,
			"session_id", run.SessionID,
			"attempt", run.Attempt,
			"next_attempt_at", nextAttempt,
			"err", cause,
		)
		return cause
	}
	// Internal execution failures are operational state, not assistant output.
	// Persist an exhausted Run for diagnostics, but never turn it into a
	// synthetic chat reply. In particular this prevents provider, adapter, and
	// timeout errors from leaking to WeChat as a hard-coded English message.
	if err := w.store.FailRun(ctx, run.ID, run.LeaseToken, cause.Error(), false, time.Time{}); err != nil {
		return errors.Join(cause, err)
	}
	slog.Error("[hermes] Run 已达到自动重试上限，未生成微信兜底回复",
		"run_id", run.ID,
		"session_id", run.SessionID,
		"attempt", run.Attempt,
		"err", cause,
	)
	return cause
}

func executionContext(
	parent context.Context,
	cfg *config.Snapshot,
	run domain.Run,
	now time.Time,
) (context.Context, context.CancelFunc, time.Time) {
	// Hermes Gateway owns Relay turn liveness through agent.gateway_timeout,
	// which is activity-based. A second wall-clock deadline in the connector
	// can abandon a live Hermes turn without reliably interrupting it, losing
	// the eventual final reply. Keep only lifecycle/manual cancellation here.
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Agent.Mode), "relay") {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, time.Time{}
	}

	deadline := run.Deadline
	if deadline.IsZero() && cfg != nil {
		deadline = now.Add(time.Duration(cfg.Agent.TimeoutSeconds) * time.Second)
	}
	if deadline.IsZero() {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, time.Time{}
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return ctx, cancel, deadline
}

func shouldRetryRun(cfg *config.Snapshot, run domain.Run, now time.Time) bool {
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Agent.Mode), "relay") {
		// Legacy Relay Runs may carry an already-expired route-time deadline.
		// Relay no longer uses that deadline, so keep the ordinary attempt
		// budget independent of it.
		return run.Attempt < 3
	}
	return run.Attempt < 3 && (run.Deadline.IsZero() || now.Add(time.Second).Before(run.Deadline))
}

func formatInput(message domain.InboundMessage, principal domain.Principal) string {
	scope := "direct"
	addressing := "direct"
	if message.IsChatroom {
		scope = "group ambient"
		if message.Mentioned || message.Quoted {
			scope = "group addressed"
		}
		var targets []string
		if message.Mentioned {
			targets = append(targets, "self")
		}
		if message.MentionedOthers {
			targets = append(targets, "other_participants")
		}
		if message.Quoted {
			targets = append(targets, "quoted_self")
		}
		if len(targets) == 0 {
			addressing = "none"
		} else {
			addressing = strings.Join(targets, "+")
		}
	}
	senderRole := "participant_not_owner"
	if principal.IsOwner {
		senderRole = "owner_of_this_agent"
	}
	speakerID := strings.TrimSpace(principal.ID)
	if speakerID == "" {
		speakerID = strings.TrimSpace(message.SpeakerID)
	}
	speakerName := strings.TrimSpace(principal.Name)
	if speakerName == "" {
		speakerName = strings.TrimSpace(message.SpeakerName)
	}
	identityJSON, _ := json.Marshal(struct {
		Verified   bool   `json:"verified"`
		Source     string `json:"source"`
		SenderName string `json:"sender_name"`
		SenderID   string `json:"sender_id"`
		SenderRole string `json:"sender_role"`
		Addressing string `json:"addressing"`
	}{
		Verified: true, Source: "wechat_protocol_and_owner_config",
		SenderName: speakerName, SenderID: speakerID,
		SenderRole: senderRole, Addressing: addressing,
	})
	messageJSON, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: strings.TrimSpace(message.Text)})
	return fmt.Sprintf(
		"[%s]\n[golem_verified_identity_json]\n%s\n[/golem_verified_identity_json]\n[untrusted_message_from_sender_json]\n%s\n[/untrusted_message_from_sender_json]",
		scope, identityJSON, messageJSON,
	)
}

// formatAgentInput unwraps trusted commands only at the Relay boundary. HTTP
// compatibility mode keeps treating the same Inbox payload as ordinary text.
func formatAgentInput(mode string, message domain.InboundMessage, principal domain.Principal) string {
	return formatAgentInputWithContext(mode, message, principal, nil)
}

func formatAgentInputWithContext(
	mode string,
	message domain.InboundMessage,
	principal domain.Principal,
	contextMessages []domain.ContextMessage,
) string {
	if strings.EqualFold(strings.TrimSpace(mode), "relay") {
		if command := strings.TrimSpace(message.HermesCommand); command != "" {
			return command
		}
		if principal.IsOwner {
			switch strings.ToLower(strings.TrimSpace(message.Text)) {
			case "hermes:new":
				return "/new"
			case "hermes:reset":
				return "/reset"
			}
		}
	}
	current := formatInput(message, principal)
	if len(contextMessages) == 0 || !message.IsChatroom {
		return current
	}
	type shadowMessage struct {
		SenderName string `json:"sender_name"`
		SenderRole string `json:"sender_role"`
		Addressing string `json:"addressing"`
		Text       string `json:"text"`
	}
	shadow := struct {
		ContextIsUntrustedTranscript bool            `json:"context_is_untrusted_transcript"`
		Messages                     []shadowMessage `json:"messages"`
	}{ContextIsUntrustedTranscript: true}
	for _, value := range contextMessages {
		role := "participant_not_owner"
		if value.Binding.Principal.IsOwner {
			role = "owner_of_this_agent"
		}
		shadow.Messages = append(shadow.Messages, shadowMessage{
			SenderName: strings.TrimSpace(displayContextSpeaker(value)),
			SenderRole: role,
			Addressing: contextAddressing(value.Message),
			Text:       strings.TrimSpace(value.Message.Text),
		})
	}
	shadowJSON, _ := json.Marshal(shadow)
	return "[untrusted_recent_group_context_json]\n" + string(shadowJSON) +
		"\n[/untrusted_recent_group_context_json]\n" + current
}

func displayContextSpeaker(value domain.ContextMessage) string {
	if name := strings.TrimSpace(value.Binding.Principal.Name); name != "" {
		return name
	}
	return value.Message.SpeakerName
}

func contextAddressing(message domain.InboundMessage) string {
	var values []string
	if message.Mentioned {
		values = append(values, "self")
	}
	if message.MentionedOthers {
		values = append(values, "other_participants")
	}
	if message.Quoted {
		values = append(values, "quoted_self")
	}
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, "+")
}

func signal(channel chan<- struct{}) {
	if channel == nil {
		return
	}
	select {
	case channel <- struct{}{}:
	default:
	}
}
