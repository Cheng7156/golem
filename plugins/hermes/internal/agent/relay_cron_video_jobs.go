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

const cronVideoJobRetention = 15 * time.Minute

type cronVideoJob struct {
	ID           string
	ScopeKey     string
	CandidateID  string
	InvocationID string
	State        string
	Result       domain.AsyncDirectOutputResult
	Failure      string
	CreatedAt    time.Time
}

type cronVideoOutcome struct {
	jobID  string
	result domain.AsyncDirectOutputResult
	err    error
}

func (g *RelayGateway) startCronVideoJob(
	run cronDirectRun,
	input cronVideoSendRequest,
) (cronVideoJob, error) {
	candidateID := strings.TrimSpace(input.CandidateID)
	invocationID := strings.TrimSpace(input.InvocationID)
	if candidateID == "" {
		return cronVideoJob{}, errors.New("candidate_id is empty")
	}
	if invocationID == "" || len(invocationID) > domain.MaxAsyncInvocationIDLength {
		return cronVideoJob{}, errors.New("cron video invocation_id is invalid")
	}
	ctx, err := g.asyncVideoContext()
	if err != nil {
		return cronVideoJob{}, err
	}
	job := newCronVideoJob(run, candidateID, invocationID)
	g.videoMu.Lock()
	g.purgeCronVideoJobsLocked(time.Now())
	if existing, ok := g.cronVideoJobs[job.ID]; ok {
		g.videoMu.Unlock()
		if existing.CandidateID != candidateID || existing.ScopeKey != job.ScopeKey {
			return cronVideoJob{}, errors.New("cron video invocation_id was reused with different input")
		}
		return existing, nil
	}
	g.cronVideoJobs[job.ID] = job
	g.videoMu.Unlock()
	go g.runCronVideoJob(ctx, job, run)
	return job, nil
}

func newCronVideoJob(
	run cronDirectRun,
	candidateID string,
	invocationID string,
) cronVideoJob {
	scopeKey := cronVideoScopeKey(run)
	digest := sha256.Sum256([]byte(scopeKey + "\x00" + invocationID))
	return cronVideoJob{
		ID: "cvjob_" + hex.EncodeToString(digest[:18]), ScopeKey: scopeKey,
		CandidateID: candidateID, InvocationID: invocationID,
		State: "pending", CreatedAt: time.Now(),
	}
}

func (g *RelayGateway) runCronVideoJob(
	ctx context.Context,
	job cronVideoJob,
	run cronDirectRun,
) {
	output, err := g.config.Videos.Select(ctx, cronVideoScope(run), job.CandidateID)
	if err != nil {
		g.finishCronVideoJob(cronVideoOutcome{jobID: job.ID, err: err})
		return
	}
	commit, err := cronVideoDirectCommit(run, job.InvocationID, output)
	if err != nil {
		g.finishCronVideoJob(cronVideoOutcome{jobID: job.ID, err: err})
		return
	}
	result, err := run.capability.CommitCronDirectOutput(ctx, commit)
	if err == nil && result.Queued && g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	g.finishCronVideoJob(cronVideoOutcome{jobID: job.ID, result: result, err: err})
}

func cronVideoDirectCommit(
	run cronDirectRun,
	invocationID string,
	output domain.VideoOutput,
) (domain.CronDirectOutputCommit, error) {
	payload, err := json.Marshal(output)
	if err != nil {
		return domain.CronDirectOutputCommit{}, err
	}
	return domain.CronDirectOutputCommit{
		Profile: run.binding.Profile, JobID: run.binding.JobID,
		DeliveryID: run.deliveryID, InvocationID: invocationID,
		Output: domain.AsyncOutput{Kind: "video", Payload: payload},
	}, nil
}

func (g *RelayGateway) finishCronVideoJob(
	outcome cronVideoOutcome,
) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	job, ok := g.cronVideoJobs[outcome.jobID]
	if !ok {
		return
	}
	job.Result, job.State = outcome.result, "completed"
	if outcome.err != nil {
		job.State, job.Failure = "failed", outcome.err.Error()
	}
	g.cronVideoJobs[outcome.jobID] = job
}

func (g *RelayGateway) cronVideoJob(jobID string, scopeKey string) (cronVideoJob, bool) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	g.purgeCronVideoJobsLocked(time.Now())
	job, ok := g.cronVideoJobs[jobID]
	return job, ok && job.ScopeKey == scopeKey
}

func cronVideoJobResponse(job cronVideoJob) map[string]any {
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

func (g *RelayGateway) purgeCronVideoJobsLocked(now time.Time) {
	for id, job := range g.cronVideoJobs {
		if job.State != "pending" && now.Sub(job.CreatedAt) >= cronVideoJobRetention {
			delete(g.cronVideoJobs, id)
		}
	}
}
