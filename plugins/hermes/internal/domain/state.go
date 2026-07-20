package domain

import "fmt"

type InboxStatus string

const (
	InboxAccepted InboxStatus = "accepted"
	InboxOrdered  InboxStatus = "ordered"
	InboxRouted   InboxStatus = "routed"
	InboxDone     InboxStatus = "done"
	InboxFailed   InboxStatus = "failed"
)

type TurnState string

const (
	TurnAccepted          TurnState = "accepted"
	TurnOrdered           TurnState = "ordered"
	TurnRouted            TurnState = "routed"
	TurnObserved          TurnState = "observed"
	TurnQueuedInteractive TurnState = "queued_interactive"
	TurnQueuedJob         TurnState = "queued_job"
	TurnRunning           TurnState = "running"
	TurnCommitting        TurnState = "committing"
	TurnCompleted         TurnState = "completed"
	TurnExpired           TurnState = "expired"
	TurnCancelled         TurnState = "cancelled"
	TurnFailed            TurnState = "failed"
	TurnDeadLetter        TurnState = "dead_letter"
)

type Route string

const (
	RouteUnset         Route = ""
	RouteObserve       Route = "observe"
	RouteChat          Route = "chat"
	RouteJob           Route = "job"
	RouteControl       Route = "control"
	RouteAsyncDelivery Route = "async_delivery"
	RouteCronDelivery  Route = "cron_delivery"
)

type Lane string

const (
	LaneControl     Lane = "control"
	LaneInteractive Lane = "interactive"
	LaneJob         Lane = "job"
	LaneBackground  Lane = "background"
)

type RunState string

const (
	RunQueued          RunState = "queued"
	RunLeased          RunState = "leased"
	RunRunning         RunState = "running"
	RunRetryWait       RunState = "retry_wait"
	RunCancelRequested RunState = "cancel_requested"
	RunCancelled       RunState = "cancelled"
	RunSucceeded       RunState = "succeeded"
	RunFailed          RunState = "failed"
	RunOrphaned        RunState = "orphaned"
)

type OutboxState string

const (
	OutboxPending    OutboxState = "pending"
	OutboxLeased     OutboxState = "leased"
	OutboxRetryWait  OutboxState = "retry_wait"
	OutboxSent       OutboxState = "sent"
	OutboxDeadLetter OutboxState = "dead_letter"
)

var turnTransitions = map[TurnState]map[TurnState]struct{}{
	TurnAccepted: {TurnOrdered: {}, TurnFailed: {}, TurnDeadLetter: {}},
	TurnOrdered:  {TurnRouted: {}, TurnFailed: {}, TurnDeadLetter: {}},
	TurnRouted: {
		TurnObserved: {}, TurnQueuedInteractive: {}, TurnQueuedJob: {},
		TurnCancelled: {}, TurnExpired: {}, TurnFailed: {},
	},
	TurnQueuedInteractive: {TurnRunning: {}, TurnCancelled: {}, TurnExpired: {}, TurnFailed: {}},
	TurnQueuedJob:         {TurnRunning: {}, TurnCancelled: {}, TurnFailed: {}},
	TurnRunning:           {TurnCommitting: {}, TurnCancelled: {}, TurnFailed: {}},
	TurnCommitting:        {TurnCompleted: {}, TurnFailed: {}},
}

var runTransitions = map[RunState]map[RunState]struct{}{
	RunQueued:          {RunLeased: {}, RunCancelRequested: {}, RunCancelled: {}},
	RunLeased:          {RunRunning: {}, RunRetryWait: {}, RunCancelRequested: {}, RunCancelled: {}, RunFailed: {}, RunOrphaned: {}},
	RunRunning:         {RunSucceeded: {}, RunRetryWait: {}, RunCancelRequested: {}, RunFailed: {}, RunOrphaned: {}},
	RunRetryWait:       {RunLeased: {}, RunCancelRequested: {}, RunCancelled: {}, RunFailed: {}},
	RunCancelRequested: {RunCancelled: {}, RunFailed: {}},
	RunOrphaned:        {RunRetryWait: {}, RunFailed: {}},
}

func ValidateTurnTransition(from, to TurnState) error {
	if _, ok := turnTransitions[from][to]; !ok {
		return fmt.Errorf("非法 Turn 状态转换: %s -> %s", from, to)
	}
	return nil
}

func ValidateRunTransition(from, to RunState) error {
	if _, ok := runTransitions[from][to]; !ok {
		return fmt.Errorf("非法 Run 状态转换: %s -> %s", from, to)
	}
	return nil
}
