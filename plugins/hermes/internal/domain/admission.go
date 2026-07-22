package domain

type RunAdmissionMode string

const (
	RunAdmissionOff    RunAdmissionMode = "off"
	RunAdmissionQueued RunAdmissionMode = "queued"
	RunAdmissionActive RunAdmissionMode = "active"
)

// RunAdmissionResult describes the scheduler side effects that must happen
// after a Turn has been routed. The store applies durable state transitions;
// the runtime is responsible for interrupting any already-running agent Run.
type RunAdmissionResult struct {
	CurrentRunID      string
	CurrentSuperseded bool
	CancelledRunIDs   []string
	InterruptRunIDs   []string
}
