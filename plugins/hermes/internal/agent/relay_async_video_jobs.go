package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

const asyncVideoJobRetention = 15 * time.Minute

type asyncVideoJob struct {
	ID           string
	TicketHash   string
	CandidateID  string
	InvocationID string
	State        string
	Result       domain.AsyncDirectOutputResult
	Failure      string
	CreatedAt    time.Time
}

func (g *RelayGateway) startAsyncVideoJob(
	ticket domain.AsyncDeliveryTicket,
	input asyncVideoSendRequest,
) (asyncVideoJob, error) {
	candidateID := strings.TrimSpace(input.CandidateID)
	invocationID := strings.TrimSpace(input.InvocationID)
	if candidateID == "" {
		return asyncVideoJob{}, errors.New("candidate_id is empty")
	}
	if invocationID == "" || len(invocationID) > domain.MaxAsyncInvocationIDLength {
		return asyncVideoJob{}, errors.New("async video invocation_id is invalid")
	}
	ctx, err := g.asyncVideoContext()
	if err != nil {
		return asyncVideoJob{}, err
	}
	job := newAsyncVideoJob(ticket, candidateID, invocationID)
	g.videoMu.Lock()
	g.purgeAsyncVideoJobsLocked(time.Now())
	if existing, ok := g.asyncVideoJobs[job.ID]; ok {
		g.videoMu.Unlock()
		if existing.CandidateID != candidateID || existing.TicketHash != ticket.TicketHash {
			return asyncVideoJob{}, errors.New("video invocation_id was reused with different input")
		}
		return existing, nil
	}
	g.asyncVideoJobs[job.ID] = job
	g.videoMu.Unlock()
	go g.runAsyncVideoJob(ctx, job, ticket)
	return job, nil
}

func newAsyncVideoJob(
	ticket domain.AsyncDeliveryTicket,
	candidateID string,
	invocationID string,
) asyncVideoJob {
	digest := sha256.Sum256([]byte(ticket.TicketHash + "\x00" + invocationID))
	return asyncVideoJob{
		ID: "avjob_" + hex.EncodeToString(digest[:18]), TicketHash: ticket.TicketHash,
		CandidateID: candidateID, InvocationID: invocationID,
		State: "pending", CreatedAt: time.Now(),
	}
}

func (g *RelayGateway) runAsyncVideoJob(
	ctx context.Context,
	job asyncVideoJob,
	ticket domain.AsyncDeliveryTicket,
) {
	output, err := g.config.Videos.Select(ctx, asyncVideoScope(ticket), job.CandidateID)
	if err != nil {
		g.finishAsyncVideoJob(job.ID, domain.AsyncDirectOutputResult{}, err)
		return
	}
	commit, err := asyncVideoDirectCommit(ticket, job.InvocationID, output)
	if err == nil {
		commitErr := error(nil)
		result, commitErr := g.config.AsyncDelivery.CommitAsyncDirectOutput(ctx, commit)
		if commitErr == nil && result.Queued && g.config.AsyncDeliveryWake != nil {
			g.config.AsyncDeliveryWake()
		}
		g.finishAsyncVideoJob(job.ID, result, commitErr)
		return
	}
	g.finishAsyncVideoJob(job.ID, domain.AsyncDirectOutputResult{}, err)
}

func asyncVideoDirectCommit(
	ticket domain.AsyncDeliveryTicket,
	invocationID string,
	output domain.VideoOutput,
) (domain.AsyncDirectOutputCommit, error) {
	payload, err := json.Marshal(output)
	if err != nil {
		return domain.AsyncDirectOutputCommit{}, err
	}
	return domain.AsyncDirectOutputCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID, RelaySessionKey: ticket.RelaySessionKey,
		ChatID: ticket.ChatID, InvocationID: invocationID,
		Output: domain.AsyncOutput{Kind: "video", Payload: payload},
	}, nil
}

func (g *RelayGateway) finishAsyncVideoJob(
	jobID string,
	result domain.AsyncDirectOutputResult,
	err error,
) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	job, ok := g.asyncVideoJobs[jobID]
	if !ok {
		return
	}
	job.Result = result
	job.State = "completed"
	if err != nil {
		job.State, job.Failure = "failed", err.Error()
	}
	g.asyncVideoJobs[jobID] = job
}

func (g *RelayGateway) asyncVideoJob(jobID, ticketHash string) (asyncVideoJob, bool) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	g.purgeAsyncVideoJobsLocked(time.Now())
	job, ok := g.asyncVideoJobs[jobID]
	return job, ok && job.TicketHash == ticketHash
}

func asyncVideoJobResponse(job asyncVideoJob) map[string]any {
	result := map[string]any{"job_id": job.ID, "state": job.State}
	if job.State == "failed" {
		result["error"] = job.Failure
	}
	if job.State == "completed" {
		result["queued"] = job.Result.Queued
		result["outbox_id"] = job.Result.OutboxID
		result["sequence"] = job.Result.Sequence
		result["direct_output_count"] = job.Result.DirectOutputCount
	}
	return result
}

func (g *RelayGateway) asyncVideoContext() (context.Context, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runtimeCtx == nil || g.closed {
		return nil, errors.New("video delivery runtime is unavailable")
	}
	return g.runtimeCtx, nil
}

func (g *RelayGateway) releaseAsyncVideos(ticketHash, chatID string) {
	if g.config.Videos != nil {
		g.config.Videos.Release(VideoScope{RunID: "async:" + ticketHash, ChatID: chatID})
	}
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	for id, job := range g.asyncVideoJobs {
		if job.TicketHash == ticketHash {
			delete(g.asyncVideoJobs, id)
		}
	}
}

func (g *RelayGateway) purgeAsyncVideoJobsLocked(now time.Time) {
	for id, job := range g.asyncVideoJobs {
		if job.State != "pending" && now.Sub(job.CreatedAt) >= asyncVideoJobRetention {
			delete(g.asyncVideoJobs, id)
		}
	}
}
