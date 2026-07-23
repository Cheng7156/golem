package domain

import (
	"errors"
	"strings"
	"time"
)

type AsyncVideoJobState string

const (
	AsyncVideoJobPending         AsyncVideoJobState = "pending"
	AsyncVideoJobRunning         AsyncVideoJobState = "running"
	AsyncVideoJobCompleted       AsyncVideoJobState = "completed"
	AsyncVideoJobWaitingDelivery AsyncVideoJobState = "waiting_delivery"
	AsyncVideoJobDelivered       AsyncVideoJobState = "delivered"
	AsyncVideoJobAmbiguous       AsyncVideoJobState = "ambiguous"
	AsyncVideoJobDeadLetter      AsyncVideoJobState = "dead_letter"
	AsyncVideoJobFailed          AsyncVideoJobState = "failed"
)

// AsyncVideoJob is the durable checkpoint for URL/candidate preparation.
// The immutable fields also form the idempotency contract for a Hermes tool
// invocation; lease fields are owned exclusively by the SQLite worker.
type AsyncVideoJob struct {
	ID            string
	TicketHash    string
	CandidateID   string
	SourceURL     string
	MediaURL      string
	Title         string
	InvocationID  string
	AutoClose     bool
	State         AsyncVideoJobState
	Stage         string
	Attempt       int
	LeaseToken    string
	LeaseUntil    time.Time
	NextAttemptAt time.Time
	Result        AsyncDirectOutputResult
	Failure       string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (j AsyncVideoJob) Validate() error {
	if strings.TrimSpace(j.ID) == "" || strings.TrimSpace(j.TicketHash) == "" {
		return errors.New("async video job identity is empty")
	}
	if strings.TrimSpace(j.InvocationID) == "" || len(j.InvocationID) > MaxAsyncInvocationIDLength {
		return errors.New("async video job invocation id is invalid")
	}
	sources := 0
	for _, value := range []string{j.CandidateID, j.SourceURL, j.MediaURL} {
		if strings.TrimSpace(value) != "" {
			sources++
		}
	}
	if sources != 1 {
		return errors.New("async video job must have exactly one source")
	}
	if len(j.SourceURL) > 4096 || len(j.MediaURL) > 4096 || len(j.Title) > 300 {
		return errors.New("async video job URL or title is too long")
	}
	return nil
}

func (j AsyncVideoJob) SameInvocation(other AsyncVideoJob) bool {
	return j.ID == other.ID && j.TicketHash == other.TicketHash &&
		j.CandidateID == other.CandidateID && j.SourceURL == other.SourceURL &&
		j.MediaURL == other.MediaURL && j.Title == other.Title &&
		j.InvocationID == other.InvocationID && j.AutoClose == other.AutoClose
}
