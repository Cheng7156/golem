package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
)

const asyncVideoJobRetention = 15 * time.Minute
const asyncVideoJobLease = 30 * time.Second
const maximumAsyncVideoJobAttempts = 3
const asyncVideoProgressAfter = 45 * time.Second

type AsyncVideoJobStore interface {
	CreateAsyncVideoJob(context.Context, domain.AsyncVideoJob) (domain.AsyncVideoJob, bool, error)
	GetAsyncVideoJob(context.Context, string) (domain.AsyncVideoJob, error)
	ListRunnableAsyncVideoJobs(context.Context, time.Time, int) ([]domain.AsyncVideoJob, error)
	LeaseAsyncVideoJob(context.Context, string, time.Time, time.Duration) (domain.AsyncVideoJob, error)
	UpdateAsyncVideoJobStage(context.Context, string, string, string) error
	ExtendAsyncVideoJobLease(context.Context, string, string, time.Time) error
	FinishAsyncVideoJob(context.Context, string, string, domain.AsyncDirectOutputResult, string) error
	RequeueAsyncVideoJob(context.Context, string, string, string, time.Time) error
	RememberAsyncVideoURLs(context.Context, string, []string, time.Time) error
	AsyncVideoURLAllowed(context.Context, string, string, time.Time) (bool, error)
	DeleteAsyncVideoURLs(context.Context, string) error
}

type asyncVideoJob = domain.AsyncVideoJob

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
	if g.config.AsyncVideoJobs != nil {
		stored, created, err := g.config.AsyncVideoJobs.CreateAsyncVideoJob(ctx, job)
		if err != nil {
			return asyncVideoJob{}, err
		}
		if created || stored.State == domain.AsyncVideoJobPending {
			go g.runAsyncVideoJob(ctx, stored, ticket)
		}
		return stored, nil
	}
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

func (g *RelayGateway) startAsyncVideoURLJob(
	ticket domain.AsyncDeliveryTicket,
	input asyncVideoSendURLRequest,
) (asyncVideoJob, error) {
	mediaURL := strings.TrimSpace(input.URL)
	title := strings.TrimSpace(input.Title)
	invocationID := strings.TrimSpace(input.InvocationID)
	if mediaURL == "" || len(mediaURL) > 4096 {
		return asyncVideoJob{}, errors.New("video URL is empty or too long")
	}
	if len(title) > 300 {
		return asyncVideoJob{}, errors.New("video title is too long")
	}
	if invocationID == "" || len(invocationID) > domain.MaxAsyncInvocationIDLength {
		return asyncVideoJob{}, errors.New("async video invocation_id is invalid")
	}
	ctx, err := g.asyncVideoContext()
	if err != nil {
		return asyncVideoJob{}, err
	}
	job := newAsyncVideoURLJob(ticket, mediaURL, title, invocationID)
	if g.config.AsyncVideoJobs != nil {
		allowed, err := g.config.AsyncVideoJobs.AsyncVideoURLAllowed(
			ctx, ticket.TicketHash, mediaURL, time.Now(),
		)
		if err != nil {
			return asyncVideoJob{}, err
		}
		if !allowed {
			return asyncVideoJob{}, errors.New("media_url was not returned by a prior URL inspection")
		}
		stored, created, err := g.config.AsyncVideoJobs.CreateAsyncVideoJob(ctx, job)
		if err != nil {
			return asyncVideoJob{}, err
		}
		if created || stored.State == domain.AsyncVideoJobPending {
			go g.runAsyncVideoJob(ctx, stored, ticket)
		}
		return stored, nil
	}
	g.videoMu.Lock()
	_, allowed := g.asyncVideoURLs[ticket.TicketHash][mediaURL]
	g.videoMu.Unlock()
	if !allowed {
		return asyncVideoJob{}, errors.New("media_url was not returned by a prior URL inspection")
	}
	g.videoMu.Lock()
	g.purgeAsyncVideoJobsLocked(time.Now())
	if existing, ok := g.asyncVideoJobs[job.ID]; ok {
		g.videoMu.Unlock()
		if existing.MediaURL != mediaURL || existing.Title != title || existing.TicketHash != ticket.TicketHash {
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
		State: domain.AsyncVideoJobPending, CreatedAt: time.Now(),
	}
}

func newAsyncVideoURLJob(
	ticket domain.AsyncDeliveryTicket,
	mediaURL string,
	title string,
	invocationID string,
) asyncVideoJob {
	digest := sha256.Sum256([]byte(ticket.TicketHash + "\x00" + invocationID))
	return asyncVideoJob{
		ID: "avjob_" + hex.EncodeToString(digest[:18]), TicketHash: ticket.TicketHash,
		MediaURL: mediaURL, Title: title, InvocationID: invocationID,
		State: domain.AsyncVideoJobPending, CreatedAt: time.Now(),
	}
}

func newInlineVideoJob(
	ticket domain.AsyncDeliveryTicket,
	sourceURL string,
	title string,
	invocationID string,
) asyncVideoJob {
	digest := sha256.Sum256([]byte(ticket.TicketHash + "\x00" + invocationID))
	return asyncVideoJob{
		ID: "avjob_" + hex.EncodeToString(digest[:18]), TicketHash: ticket.TicketHash,
		SourceURL: sourceURL, Title: title, InvocationID: invocationID,
		AutoClose: true, State: domain.AsyncVideoJobPending, CreatedAt: time.Now(),
	}
}

func (g *RelayGateway) runAsyncVideoJob(
	ctx context.Context,
	job asyncVideoJob,
	ticket domain.AsyncDeliveryTicket,
) {
	stopLease := func() {}
	if g.config.AsyncVideoJobs != nil {
		leased, err := g.config.AsyncVideoJobs.LeaseAsyncVideoJob(
			ctx, job.ID, time.Now(), asyncVideoJobLease,
		)
		if err != nil {
			return
		}
		job = leased
		stopLease = g.maintainAsyncVideoJobLease(ctx, leased)
	}
	defer stopLease()
	stopProgress := g.scheduleAsyncVideoProgress(ctx, job, ticket)
	defer stopProgress()
	candidateID := job.CandidateID
	if job.SourceURL != "" {
		g.setAsyncVideoJobStage(ctx, job, "inspecting")
		inspection, err := g.config.Videos.InspectURL(
			ctx, asyncVideoScope(ticket), job.SourceURL,
		)
		if err != nil {
			stopLease()
			g.completeAsyncVideoJob(ctx, job, ticket, domain.AsyncDirectOutputResult{}, err)
			return
		}
		if err := g.rememberAsyncVideoURLs(ctx, ticket.TicketHash, inspection); err != nil {
			stopLease()
			g.completeAsyncVideoJob(ctx, job, ticket, domain.AsyncDirectOutputResult{}, err)
			return
		}
		mediaURL, err := automaticVideoInspectionURL(inspection)
		if err != nil {
			stopLease()
			g.completeAsyncVideoJob(ctx, job, ticket, domain.AsyncDirectOutputResult{}, err)
			return
		}
		job.MediaURL = mediaURL
	}
	if candidateID == "" {
		g.setAsyncVideoJobStage(ctx, job, "resolving")
		candidate, err := g.config.Videos.ResolveURL(ctx, asyncVideoScope(ticket), VideoURLInput{
			URL: job.MediaURL, Title: job.Title,
		})
		if err != nil {
			stopLease()
			g.completeAsyncVideoJob(ctx, job, ticket, domain.AsyncDirectOutputResult{}, err)
			return
		}
		candidateID = candidate.ID
	}
	g.setAsyncVideoJobStage(ctx, job, "preparing")
	output, err := g.config.Videos.Select(ctx, asyncVideoScope(ticket), candidateID)
	if err != nil {
		stopLease()
		g.completeAsyncVideoJob(ctx, job, ticket, domain.AsyncDirectOutputResult{}, err)
		return
	}
	commit, err := asyncVideoDirectCommit(ticket, job.InvocationID, output)
	if err == nil {
		g.setAsyncVideoJobStage(ctx, job, "committing")
		commitErr := error(nil)
		result, commitErr := g.config.AsyncDelivery.CommitAsyncDirectOutput(ctx, commit)
		if commitErr == nil && result.Queued && g.config.AsyncDeliveryWake != nil {
			g.config.AsyncDeliveryWake()
		}
		stopLease()
		g.completeAsyncVideoJob(ctx, job, ticket, result, commitErr)
		return
	}
	stopLease()
	g.completeAsyncVideoJob(ctx, job, ticket, domain.AsyncDirectOutputResult{}, err)
}

func automaticVideoInspectionURL(inspection VideoURLInspection) (string, error) {
	switch inspection.Kind {
	case "video":
		if value := strings.TrimSpace(inspection.FinalURL); value != "" {
			return value, nil
		}
	case "url":
		if len(inspection.Candidates) > 0 {
			if value := strings.TrimSpace(inspection.Candidates[0].URL); value != "" {
				return value, nil
			}
		}
	case "json":
		if len(inspection.Candidates) == 1 {
			return strings.TrimSpace(inspection.Candidates[0].URL), nil
		}
		if len(inspection.Candidates) >= 2 && inspection.Candidates[0].Score >= 45 &&
			inspection.Candidates[0].Score-inspection.Candidates[1].Score >= 20 {
			return strings.TrimSpace(inspection.Candidates[0].URL), nil
		}
	}
	return "", errors.New("could not determine one unambiguous video URL")
}

func (g *RelayGateway) completeAsyncVideoJob(
	ctx context.Context,
	job asyncVideoJob,
	ticket domain.AsyncDeliveryTicket,
	result domain.AsyncDirectOutputResult,
	err error,
) {
	interrupted := ctx.Err() != nil
	retrying := err != nil && retryableAsyncVideoJobError(err) &&
		job.Attempt < maximumAsyncVideoJobAttempts
	if job.AutoClose && !interrupted && !retrying {
		if err != nil {
			if failureErr := g.commitInlineVideoFailure(ctx, job, ticket, err); failureErr != nil {
				slog.Warn("[hermes] could not queue inline video failure",
					"job_id", job.ID, "err", failureErr)
			}
		}
		if closeErr := g.closeInlineVideoTicket(ctx, ticket); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	g.finishAsyncVideoJob(job, result, err, interrupted)
}

func (g *RelayGateway) commitInlineVideoFailure(
	ctx context.Context,
	job asyncVideoJob,
	ticket domain.AsyncDeliveryTicket,
	failure error,
) error {
	message := strings.TrimSpace(failure.Error())
	if len([]rune(message)) > 300 {
		message = string([]rune(message)[:300])
	}
	payload, err := json.Marshal(domain.TextOutput{
		Content: "视频任务 " + shortAsyncVideoJobID(job.ID) + " 失败：" + message,
	})
	if err != nil {
		return err
	}
	_, err = g.config.AsyncDelivery.CommitAsyncDirectOutput(ctx, domain.AsyncDirectOutputCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID, RelaySessionKey: ticket.RelaySessionKey,
		ChatID: ticket.ChatID, InvocationID: job.ID + ":failure",
		Output: domain.AsyncOutput{Kind: "text", Payload: payload},
	})
	if err == nil && g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	return err
}

func (g *RelayGateway) closeInlineVideoTicket(
	ctx context.Context,
	ticket domain.AsyncDeliveryTicket,
) error {
	_, err := g.config.AsyncDelivery.CommitAsyncDelivery(ctx, domain.AsyncDeliveryCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID, RelaySessionKey: ticket.RelaySessionKey,
		ChatID: ticket.ChatID, Silent: true,
	})
	if err == nil {
		g.releaseAsyncVideos(ticket.TicketHash, ticket.ChatID)
	}
	return err
}

func (g *RelayGateway) scheduleAsyncVideoProgress(
	ctx context.Context,
	job asyncVideoJob,
	ticket domain.AsyncDeliveryTicket,
) func() {
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopped) }) }
	go func() {
		timer := time.NewTimer(asyncVideoProgressAfter)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-stopped:
			return
		case <-timer.C:
		}
		current, ok := g.asyncVideoJob(job.ID, job.TicketHash)
		if !ok || (current.State != domain.AsyncVideoJobPending &&
			current.State != domain.AsyncVideoJobRunning) {
			return
		}
		if err := g.commitAsyncVideoProgress(ctx, current, ticket); err != nil && ctx.Err() == nil {
			slog.Warn("[hermes] could not queue async video progress",
				"job_id", job.ID, "err", err)
		}
	}()
	return stop
}

func (g *RelayGateway) commitAsyncVideoProgress(
	ctx context.Context,
	job asyncVideoJob,
	ticket domain.AsyncDeliveryTicket,
) error {
	stage := asyncVideoProgressLabel(job.Stage)
	payload, err := json.Marshal(domain.TextOutput{
		Content: "视频任务 " + shortAsyncVideoJobID(job.ID) + " 仍在" + stage + "，完成后会直接发到当前会话。",
	})
	if err != nil {
		return err
	}
	commit := domain.AsyncDirectOutputCommit{
		TicketHash: ticket.TicketHash, Profile: ticket.Profile,
		ProducerEpoch: ticket.ProducerEpoch, DelegationID: ticket.DelegationID,
		HermesSessionID: ticket.HermesSessionID, RelaySessionKey: ticket.RelaySessionKey,
		ChatID: ticket.ChatID, InvocationID: job.ID + ":progress:1",
		Output: domain.AsyncOutput{Kind: "text", Payload: payload},
	}
	result, err := g.config.AsyncDelivery.CommitAsyncDirectOutput(ctx, commit)
	if err == nil && result.Queued && g.config.AsyncDeliveryWake != nil {
		g.config.AsyncDeliveryWake()
	}
	return err
}

func asyncVideoProgressLabel(stage string) string {
	switch stage {
	case "inspecting":
		return "检查链接内容"
	case "resolving":
		return "解析视频地址"
	case "preparing":
		return "下载或处理视频"
	case "committing":
		return "准备发送视频"
	case "retry_wait":
		return "等待自动重试"
	default:
		return "处理中"
	}
}

func shortAsyncVideoJobID(jobID string) string {
	value := strings.TrimSpace(jobID)
	if len(value) <= 12 {
		return value
	}
	return value[len(value)-8:]
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
	leased asyncVideoJob,
	result domain.AsyncDirectOutputResult,
	err error,
	interrupted bool,
) {
	if g.config.AsyncVideoJobs != nil {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if interrupted {
			message := "video job interrupted by plugin shutdown"
			if err != nil {
				message = err.Error()
			}
			if retryErr := g.config.AsyncVideoJobs.RequeueAsyncVideoJob(
				finishCtx, leased.ID, leased.LeaseToken, message, time.Now(),
			); retryErr != nil {
				slog.Warn("[hermes] could not requeue interrupted async video job",
					"job_id", leased.ID, "err", retryErr)
			}
			return
		}
		if err != nil && retryableAsyncVideoJobError(err) && leased.Attempt < maximumAsyncVideoJobAttempts {
			delay := time.Duration(1<<(leased.Attempt-1)) * time.Second
			if retryErr := g.config.AsyncVideoJobs.RequeueAsyncVideoJob(
				finishCtx, leased.ID, leased.LeaseToken, err.Error(), time.Now().Add(delay),
			); retryErr != nil {
				slog.Warn("[hermes] could not retry async video job",
					"job_id", leased.ID, "err", retryErr)
			}
			return
		}
		failure := ""
		if err != nil {
			failure = err.Error()
		}
		if finishErr := g.config.AsyncVideoJobs.FinishAsyncVideoJob(
			finishCtx, leased.ID, leased.LeaseToken, result, failure,
		); finishErr != nil {
			slog.Warn("[hermes] could not finish durable async video job",
				"job_id", leased.ID, "err", finishErr)
		}
		return
	}
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	job, ok := g.asyncVideoJobs[leased.ID]
	if !ok {
		return
	}
	job.Result = result
	job.State = domain.AsyncVideoJobCompleted
	if err != nil {
		job.State, job.Failure = domain.AsyncVideoJobFailed, err.Error()
	}
	g.asyncVideoJobs[leased.ID] = job
}

func (g *RelayGateway) asyncVideoJob(jobID, ticketHash string) (asyncVideoJob, bool) {
	if g.config.AsyncVideoJobs != nil {
		job, err := g.config.AsyncVideoJobs.GetAsyncVideoJob(context.Background(), jobID)
		return job, err == nil && job.TicketHash == ticketHash
	}
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	g.purgeAsyncVideoJobsLocked(time.Now())
	job, ok := g.asyncVideoJobs[jobID]
	return job, ok && job.TicketHash == ticketHash
}

func asyncVideoJobResponse(job asyncVideoJob) map[string]any {
	state := job.State
	if state == domain.AsyncVideoJobRunning {
		state = domain.AsyncVideoJobPending
	}
	result := map[string]any{"job_id": job.ID, "state": state}
	result["stage"] = job.Stage
	result["attempt"] = job.Attempt
	if job.State == domain.AsyncVideoJobFailed || job.State == domain.AsyncVideoJobAmbiguous ||
		job.State == domain.AsyncVideoJobDeadLetter {
		result["error"] = job.Failure
	}
	if job.Result.OutboxID != "" {
		result["queued"] = job.Result.Queued
		result["outbox_id"] = job.Result.OutboxID
		result["sequence"] = job.Result.Sequence
		result["direct_output_count"] = job.Result.DirectOutputCount
	}
	return result
}

func (g *RelayGateway) setAsyncVideoJobStage(
	ctx context.Context,
	job asyncVideoJob,
	stage string,
) {
	if g.config.AsyncVideoJobs == nil {
		return
	}
	if err := g.config.AsyncVideoJobs.UpdateAsyncVideoJobStage(
		ctx, job.ID, job.LeaseToken, stage,
	); err != nil && ctx.Err() == nil {
		slog.Warn("[hermes] could not update async video job stage",
			"job_id", job.ID, "stage", stage, "err", err)
	}
}

func retryableAsyncVideoJobError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"deadline exceeded", "connection reset", "connection refused",
		"temporary failure", "timeout", "http 408", "http 425", "http 429",
		"http 500", "http 502", "http 503", "http 504",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (g *RelayGateway) maintainAsyncVideoJobLease(
	ctx context.Context,
	job asyncVideoJob,
) func() {
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopped) }) }
	go func() {
		ticker := time.NewTicker(asyncVideoJobLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopped:
				return
			case now := <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := g.config.AsyncVideoJobs.ExtendAsyncVideoJobLease(
					heartbeatCtx, job.ID, job.LeaseToken, now.Add(asyncVideoJobLease),
				)
				cancel()
				if err != nil {
					slog.Warn("[hermes] async video job lease heartbeat failed",
						"job_id", job.ID, "err", err)
					return
				}
			}
		}
	}()
	return stop
}

func (g *RelayGateway) runAsyncVideoRecovery(ctx context.Context) {
	g.dispatchRunnableAsyncVideoJobs(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.dispatchRunnableAsyncVideoJobs(ctx)
		}
	}
}

func (g *RelayGateway) dispatchRunnableAsyncVideoJobs(ctx context.Context) {
	jobs, err := g.config.AsyncVideoJobs.ListRunnableAsyncVideoJobs(ctx, time.Now(), 32)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("[hermes] could not list durable async video jobs", "err", err)
		}
		return
	}
	for _, job := range jobs {
		ticket, ticketErr := g.config.AsyncDelivery.GetAsyncDelivery(ctx, job.TicketHash)
		if ticketErr != nil {
			slog.Warn("[hermes] could not restore async video ticket",
				"job_id", job.ID, "err", ticketErr)
			continue
		}
		go g.runAsyncVideoJob(ctx, job, ticket)
	}
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
	if g.config.AsyncVideoJobs != nil {
		if err := g.config.AsyncVideoJobs.DeleteAsyncVideoURLs(
			context.Background(), ticketHash,
		); err != nil {
			slog.Warn("[hermes] could not release async video URL grants",
				"ticket_hash", ticketHash, "err", err)
		}
		return
	}
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	for id, job := range g.asyncVideoJobs {
		if job.TicketHash == ticketHash {
			delete(g.asyncVideoJobs, id)
		}
	}
	delete(g.asyncVideoURLs, ticketHash)
}

func (g *RelayGateway) rememberAsyncVideoURLs(
	ctx context.Context,
	ticketHash string,
	inspection VideoURLInspection,
) error {
	urls := make(map[string]struct{}, len(inspection.Candidates)+1)
	if value := strings.TrimSpace(inspection.FinalURL); value != "" {
		urls[value] = struct{}{}
	}
	for _, candidate := range inspection.Candidates {
		if value := strings.TrimSpace(candidate.URL); value != "" && len(urls) < 64 {
			urls[value] = struct{}{}
		}
	}
	if len(urls) == 0 {
		return nil
	}
	if g.config.AsyncVideoJobs != nil {
		values := make([]string, 0, len(urls))
		for value := range urls {
			values = append(values, value)
		}
		return g.config.AsyncVideoJobs.RememberAsyncVideoURLs(
			ctx, ticketHash, values, time.Now().Add(asyncVideoJobRetention),
		)
	}
	g.videoMu.Lock()
	defer g.videoMu.Unlock()
	allowed := g.asyncVideoURLs[ticketHash]
	if allowed == nil {
		allowed = make(map[string]struct{}, len(urls))
		g.asyncVideoURLs[ticketHash] = allowed
	}
	for value := range urls {
		if len(allowed) >= 64 {
			break
		}
		allowed[value] = struct{}{}
	}
	return nil
}

func (g *RelayGateway) purgeAsyncVideoJobsLocked(now time.Time) {
	for id, job := range g.asyncVideoJobs {
		if job.State != domain.AsyncVideoJobPending && now.Sub(job.CreatedAt) >= asyncVideoJobRetention {
			delete(g.asyncVideoJobs, id)
		}
	}
}
