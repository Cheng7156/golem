package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type Capability string

type Scope struct {
	RunID        string
	SessionID    string
	Principal    domain.Principal
	Lane         domain.Lane
	Capabilities []Capability
}

func (s Scope) Has(capability Capability) bool {
	for _, candidate := range s.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

type Spec struct {
	Name                 string          `json:"name"`
	Version              string          `json:"version"`
	Description          string          `json:"description,omitempty"`
	InputSchema          json.RawMessage `json:"input_schema"`
	OutputSchema         json.RawMessage `json:"output_schema,omitempty"`
	RequiredCapabilities []Capability    `json:"required_capabilities,omitempty"`
	ReadOnly             bool            `json:"read_only"`
	SideEffect           bool            `json:"side_effect"`
	DefaultTimeout       time.Duration   `json:"default_timeout"`
	ConcurrencyLimit     int             `json:"concurrency_limit"`
	MaxResultBytes       int             `json:"max_result_bytes,omitempty"`
	SupportsCheckpoint   bool            `json:"supports_checkpoint"`
}

func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Version) == "" {
		return errors.New("tool spec requires name and version")
	}
	if len(s.InputSchema) == 0 || !json.Valid(s.InputSchema) {
		return errors.New("tool input_schema must be valid JSON")
	}
	if len(s.OutputSchema) > 0 && !json.Valid(s.OutputSchema) {
		return errors.New("tool output_schema must be valid JSON")
	}
	if s.ReadOnly && s.SideEffect {
		return errors.New("tool cannot be both read-only and side-effecting")
	}
	if s.DefaultTimeout <= 0 {
		return errors.New("tool default timeout must be positive")
	}
	if s.ConcurrencyLimit <= 0 {
		return errors.New("tool concurrency limit must be positive")
	}
	return nil
}

type Call struct {
	InvocationID string          `json:"invocation_id"`
	RunID        string          `json:"run_id"`
	Name         string          `json:"name"`
	Arguments    json.RawMessage `json:"arguments"`
	Deadline     time.Time       `json:"deadline,omitempty"`
}

func (c Call) Validate() error {
	if strings.TrimSpace(c.InvocationID) == "" || strings.TrimSpace(c.Name) == "" {
		return errors.New("tool call requires invocation_id and name")
	}
	if len(c.Arguments) == 0 || !json.Valid(c.Arguments) {
		return errors.New("tool arguments must be valid JSON")
	}
	return nil
}

type Invocation struct {
	Call
	Scope Scope
}

type Result struct {
	InvocationID string          `json:"invocation_id"`
	Output       json.RawMessage `json:"output,omitempty"`
	Error        string          `json:"error,omitempty"`
	Retryable    bool            `json:"retryable,omitempty"`
}

type Tool interface {
	Spec() Spec
	Invoke(context.Context, Invocation) (Result, error)
}

type Registry interface {
	Specs(context.Context, Scope) []Spec
	Invoke(context.Context, Scope, Call) (Result, error)
}
