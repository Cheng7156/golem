package domain

import "time"

type RelayRunResult struct {
	ProposalID   string
	InvocationID string
	RunID        string
	ResultKind   string
	ResultHash   string
	OutboxIDs    []string
	CreatedAt    time.Time
}

func (r RelayRunResult) Validate() error {
	if r.ProposalID == "" || r.InvocationID == "" || r.RunID == "" || r.ResultHash == "" {
		return ErrInvalidRelayRunResult
	}
	switch r.ResultKind {
	case "visible_reply", "observe", "effect_only":
		return nil
	default:
		return ErrInvalidRelayRunResult
	}
}

var ErrInvalidRelayRunResult = errString("invalid relay run result")

type errString string

func (e errString) Error() string { return string(e) }
