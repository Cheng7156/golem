package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/tool"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type RunRequest struct {
	RunID                string
	SessionID            string
	SessionNamespace     string
	Principal            domain.Principal
	Lane                 domain.Lane
	BaseSessionVersion   uint64
	Input                string
	SystemPrompt         string
	ConversationSnapshot []Message
	Model                string
	ToolSpecs            []tool.Spec
	Deadline             time.Time
	Checkpoint           json.RawMessage
	Revision             int
	ChatType             string
	ChatName             string
	MessageID            string
	RequireVisibleReply  bool
	Media                []domain.InboundMedia
}

type EventKind string

const (
	EventRunAccepted       EventKind = "run_accepted"
	EventThinkingDelta     EventKind = "thinking_delta"
	EventReplyProposed     EventKind = "reply_proposed"
	EventEffectProposed    EventKind = "effect_proposed"
	EventProgress          EventKind = "progress_proposed"
	EventToolCallRequested EventKind = "tool_call_requested"
	EventToolCallCancelled EventKind = "tool_call_cancelled"
	EventCheckpoint        EventKind = "checkpoint_saved"
	EventRunCompleted      EventKind = "run_completed"
	EventRunFailed         EventKind = "run_failed"
)

type OutputProposal struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func NewTextProposal(content string) (OutputProposal, error) {
	payload, err := json.Marshal(domain.TextOutput{Content: strings.TrimSpace(content)})
	if err != nil {
		return OutputProposal{}, err
	}
	return OutputProposal{Kind: "text", Payload: payload}, nil
}

func (p OutputProposal) Validate() error {
	if strings.TrimSpace(p.Kind) == "" {
		return errors.New("output proposal kind is empty")
	}
	if len(p.Payload) == 0 || !json.Valid(p.Payload) {
		return errors.New("output proposal payload must be valid JSON")
	}
	return nil
}

type Event struct {
	Kind          EventKind
	RunID         string
	Sequence      uint64
	CorrelationID string
	Timestamp     time.Time
	Text          string
	Proposal      *OutputProposal
	ToolCall      *tool.Call
	Checkpoint    json.RawMessage
	Err           error
}

type CommandKind string

const (
	CommandCancel     CommandKind = "cancel_run"
	CommandRevise     CommandKind = "revise_run"
	CommandToolResult CommandKind = "tool_result"
)

type Command struct {
	Kind       CommandKind
	RunID      string
	Revision   int
	Input      string
	ToolResult *tool.Result
}

type Stream interface {
	Recv(context.Context) (Event, error)
	Send(context.Context, Command) error
	Cancel() error
	Close() error
}

type Health struct {
	Ready   bool
	Status  string
	Details map[string]any
}

type Gateway interface {
	Start(context.Context, RunRequest) (Stream, error)
	Health(context.Context) Health
	Close(context.Context) error
}

type Engine interface {
	Start(context.Context, RunRequest) (Stream, error)
	Health(context.Context) Health
	Close(context.Context) error
}

type Canceller interface {
	CancelRun(context.Context, string) error
}

type Runtime struct {
	gateway Gateway
}

func NewRuntime(gateway Gateway) (*Runtime, error) {
	if gateway == nil {
		return nil, errors.New("agent runtime requires a gateway")
	}
	return &Runtime{gateway: gateway}, nil
}

func (r *Runtime) Start(ctx context.Context, request RunRequest) (Stream, error) {
	return r.gateway.Start(ctx, request)
}

func (r *Runtime) Health(ctx context.Context) Health {
	return r.gateway.Health(ctx)
}

func (r *Runtime) CancelRun(ctx context.Context, runID string) error {
	canceller, ok := r.gateway.(Canceller)
	if !ok {
		return ErrCommandUnsupported
	}
	return canceller.CancelRun(ctx, runID)
}

func (r *Runtime) Close(ctx context.Context) error {
	return r.gateway.Close(ctx)
}

type ChannelStream struct {
	cancel context.CancelFunc
	events <-chan Event
	send   func(context.Context, Command) error
	once   sync.Once
}

func NewChannelStream(
	cancel context.CancelFunc,
	events <-chan Event,
	sender ...func(context.Context, Command) error,
) *ChannelStream {
	var send func(context.Context, Command) error
	if len(sender) > 0 {
		send = sender[0]
	}
	return &ChannelStream{cancel: cancel, events: events, send: send}
}

func (s *ChannelStream) Recv(ctx context.Context) (Event, error) {
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			return Event{}, io.EOF
		}
		return event, nil
	}
}

func (s *ChannelStream) Send(ctx context.Context, command Command) error {
	if s.send == nil {
		return ErrCommandUnsupported
	}
	return s.send(ctx, command)
}

func (s *ChannelStream) Cancel() error {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	return nil
}

func (s *ChannelStream) Close() error {
	return s.Cancel()
}

var (
	ErrNoReply            = errors.New("agent completed without a reply")
	ErrCommandUnsupported = errors.New("agent stream command is unsupported")
	ErrRunNotActive       = errors.New("agent run is not active")
)
