package domain

type RelayInvocationStatus struct {
	InvocationID string
	RunID        string
	RunState     RunState
	ProposalID   string
	ResultKind   string
	ResultHash   string
	OutboxIDs    []string
	LastError    string
}
