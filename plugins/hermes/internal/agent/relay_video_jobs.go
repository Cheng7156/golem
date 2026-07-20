package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const videoJobQueueSize = 8

type videoJob struct {
	ID              string    `json:"job_id"`
	RunID           string    `json:"-"`
	State           string    `json:"state"`
	Staged          bool      `json:"staged"`
	Fallback        bool      `json:"fallback"`
	FailureReason   string    `json:"failure_reason,omitempty"`
	FallbackURL     string    `json:"fallback_url,omitempty"`
	EffectOnlyToken string    `json:"effect_only_token,omitempty"`
	CreatedAt       time.Time `json:"-"`
}

type videoWork struct {
	jobID       string
	candidateID string
}

func (g *RelayGateway) enqueueVideoJob(run *relayRun, candidateID string) (videoJob, error) {
	jobID, err := newVideoJobID()
	if err != nil {
		return videoJob{}, err
	}
	job := videoJob{ID: jobID, RunID: run.request.RunID, State: "pending", CreatedAt: time.Now()}
	g.videoMu.Lock()
	g.videoJobs[jobID] = job
	g.videoMu.Unlock()
	select {
	case run.videoQueue <- videoWork{jobID: jobID, candidateID: candidateID}:
		return job, nil
	default:
		g.deleteVideoJob(jobID)
		return videoJob{}, errors.New("video preparation queue is full")
	}
}

func (r *relayRun) processVideoJobs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case work := <-r.videoQueue:
			r.processVideoJob(ctx, work)
		}
	}
}

func (r *relayRun) processVideoJob(ctx context.Context, work videoWork) {
	output, err := r.engine.config.Videos.Select(ctx, videoScope(r), work.candidateID)
	if err != nil {
		r.completeFailedVideoJob(work.jobID, err)
		return
	}
	proposal, err := marshalVideoProposal(output)
	if err == nil {
		err = r.stageEffect(proposal)
	}
	if err != nil {
		r.completeFailedVideoJob(work.jobID, err)
		return
	}
	r.engine.completeVideoJob(work.jobID, videoJob{
		State: "completed", Staged: true, EffectOnlyToken: relayEffectOnlyToken,
	})
}

func (r *relayRun) completeFailedVideoJob(jobID string, failure error) {
	slog.Warn("[hermes] video preparation failed", "run_id", r.request.RunID, "err", failure)
	fallbackURL := videoFallbackURL(failure)
	if !r.engine.config.VideoLinkFallback {
		r.engine.completeVideoJob(jobID, videoJob{
			State: "failed", FailureReason: failure.Error(), FallbackURL: fallbackURL,
		})
		return
	}
	proposal, err := NewTextProposal(videoFailureTextWithURL(failure, fallbackURL))
	if err == nil {
		err = r.stageEffect(proposal)
	}
	if err != nil {
		r.engine.completeVideoJob(jobID, videoJob{State: "failed", FailureReason: err.Error()})
		return
	}
	r.engine.completeVideoJob(jobID, videoJob{
		State: "completed", Staged: true, Fallback: true,
		FailureReason: failure.Error(), FallbackURL: fallbackURL,
		EffectOnlyToken: relayEffectOnlyToken,
	})
}

func (g *RelayGateway) videoJob(jobID, runID string) (videoJob, error) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	job, exists := g.videoJobs[jobID]
	if !exists || job.RunID != runID {
		return videoJob{}, errors.New("video job was not found")
	}
	return job, nil
}

func (g *RelayGateway) completeVideoJob(jobID string, result videoJob) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	job, exists := g.videoJobs[jobID]
	if !exists {
		return
	}
	result.ID, result.RunID, result.CreatedAt = job.ID, job.RunID, job.CreatedAt
	g.videoJobs[jobID] = result
}

func (g *RelayGateway) deleteVideoJob(jobID string) {
	g.videoMu.Lock()
	delete(g.videoJobs, jobID)
	g.videoMu.Unlock()
}

func (g *RelayGateway) deleteRunVideoJobs(runID string) {
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	for id, job := range g.videoJobs {
		if job.RunID == runID {
			delete(g.videoJobs, id)
		}
	}
}

func newVideoJobID() (string, error) {
	var data [18]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create video job id: %w", err)
	}
	return "vjob_" + base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func videoFallbackURL(err error) string {
	type fallbackError interface{ Fallback() string }
	var value fallbackError
	if errors.As(err, &value) {
		return strings.TrimSpace(value.Fallback())
	}
	return ""
}

func videoFailureTextWithURL(err error, fallbackURL string) string {
	message := "视频发送失败：" + err.Error()
	if fallbackURL != "" {
		message += "\n原链接：" + fallbackURL
	}
	return message
}
