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
	media := append([]domain.InboundMedia(nil), incoming.Media...)
	if w.media != nil {
		media, err = w.media.Resolve(runCtx, media)
		if err != nil {
			return w.finishFailure(parent, run, fmt.Errorf("resolve inbound media: %w", err))
		}
	}
	scope := toolScope(run, inbox.Binding.Principal)
	stream, err := w.engine.Start(runCtx, agent.RunRequest{
		RunID:              run.ID,
		SessionID:          run.SessionID,
		Principal:          inbox.Binding.Principal,
		Lane:               run.Lane,
		BaseSessionVersion: turn.BaseSessionVersion,
		Input:              formatAgentInput(cfg.Agent.Mode, incoming),
		SystemPrompt:       cfg.Agent.SystemPrompt,
		Model:              cfg.Agent.Model,
		ToolSpecs:          w.tools.Specs(runCtx, scope),
		Deadline:           deadline,
		Checkpoint:         append(json.RawMessage(nil), run.Checkpoint...),
		Revision:           run.Revision,
		ChatType:           chatType(incoming),
		ChatName:           incoming.RoomName,
		MessageID:          inbox.ID,
		Media:              media,
	})
	if err != nil {
		return w.finishFailure(parent, run, err)
	}
	defer stream.Close()

	var drafts []domain.OutboxDraft
	var lastSequence uint64
	completed := false
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
	if _, err := w.store.CommitRunSuccess(parent, run.ID, run.LeaseToken, drafts); err != nil {
		return err
	}
	signal(w.outputWake)
	return nil
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

func formatInput(message domain.InboundMessage) string {
	scope := "direct"
	if message.IsChatroom {
		scope = "group ambient"
		if message.Mentioned || message.Quoted {
			scope = "group addressed"
		}
	}
	return fmt.Sprintf(
		"[%s]\nsender: %s (%s)\nmessage: %s",
		scope,
		emptyDash(message.SpeakerName),
		emptyDash(message.SpeakerID),
		strings.TrimSpace(message.Text),
	)
}

// formatAgentInput unwraps trusted commands only at the Relay boundary. HTTP
// compatibility mode keeps treating the same Inbox payload as ordinary text.
func formatAgentInput(mode string, message domain.InboundMessage) string {
	if strings.EqualFold(strings.TrimSpace(mode), "relay") {
		if command := strings.TrimSpace(message.HermesCommand); command != "" {
			return command
		}
		switch strings.ToLower(strings.TrimSpace(message.Text)) {
		case "hermes:new":
			return "/new"
		case "hermes:reset":
			return "/reset"
		}
	}
	return formatInput(message)
}

func emptyDash(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "-"
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
