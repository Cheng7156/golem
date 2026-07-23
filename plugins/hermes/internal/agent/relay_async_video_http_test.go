package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/store/sqlite"
)

type asyncVideoCapability struct {
	searchScope  VideoScope
	inspectScope VideoScope
	selectScope  VideoScope
	inspectURL   string
	resolvedURL  VideoURLInput
	searchError  error
}

func (f *asyncVideoCapability) Search(
	_ context.Context,
	scope VideoScope,
	_ VideoSearchInput,
) (VideoSearchResult, error) {
	f.searchScope = scope
	if f.searchError != nil {
		return VideoSearchResult{}, f.searchError
	}
	return VideoSearchResult{Candidates: []VideoCandidate{{
		ID: "video-candidate", ProviderID: "configured-provider", Title: "sample",
	}}}, nil
}

func TestAsyncVideoSearchRejectsInvalidProviderAsClientError(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	delivery := asyncStickerCapability(plainTicket)
	videos := &asyncVideoCapability{searchError: fmt.Errorf(
		"%w: video provider not found: xjj_stream", ErrInvalidVideoSearch,
	)}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: delivery, Videos: videos,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := newAsyncVideoServer(gateway)
	defer server.Close()

	status, body := postCapability(t, server.URL+asyncVideoSearchPath, asyncVideoSearchRequest{
		asyncBoundRequest: asyncBoundForTicket(plainTicket, delivery.ticket),
		Category:          "xjj", ProviderID: "xjj_stream", Limit: 1,
	})
	if status != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("search status=%d body=%#v", status, body)
	}
}

func (f *asyncVideoCapability) ResolveURL(
	_ context.Context,
	_ VideoScope,
	input VideoURLInput,
) (VideoCandidate, error) {
	f.resolvedURL = input
	return VideoCandidate{ID: "url-video-candidate", ProviderID: "direct_url"}, nil
}

func (f *asyncVideoCapability) InspectURL(
	_ context.Context,
	scope VideoScope,
	rawURL string,
) (VideoURLInspection, error) {
	f.inspectScope, f.inspectURL = scope, rawURL
	return VideoURLInspection{
		Kind: "json", Document: map[string]any{"video": "https://cdn.example/video.mp4"},
		Candidates: []VideoURLReference{{URL: "https://cdn.example/video.mp4"}},
	}, nil
}

func (f *asyncVideoCapability) Select(
	_ context.Context,
	scope VideoScope,
	_ string,
) (domain.VideoOutput, error) {
	f.selectScope = scope
	return domain.VideoOutput{
		ObjectID: "video-object", ThumbObjectID: "thumb-object", Duration: 3,
	}, nil
}

func TestAsyncVideoURLInspectionAndSendUseTicketScope(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	delivery := asyncStickerCapability(plainTicket)
	videos := new(asyncVideoCapability)
	jobs, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("open video job store: %v", err)
	}
	defer jobs.Close()
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: delivery, Videos: videos,
		AsyncVideoJobs: jobs,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	gateway.runtimeCtx = context.Background()
	server := newAsyncVideoServer(gateway)
	defer server.Close()
	bound := asyncBoundForTicket(plainTicket, delivery.ticket)

	status, body := postCapability(t, server.URL+asyncVideoSendURLPath, asyncVideoSendURLRequest{
		asyncBoundRequest: bound, URL: "https://cdn.example/not-inspected.mp4",
		InvocationID: "call-uninspected",
	})
	if status != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("uninspected send status=%d body=%#v", status, body)
	}

	status, body = postCapability(t, server.URL+asyncVideoInspectPath, asyncVideoInspectRequest{
		asyncBoundRequest: bound, URL: "https://api.example/video",
	})
	if status != http.StatusOK || body["kind"] != "json" {
		t.Fatalf("inspect status=%d body=%#v", status, body)
	}
	status, body = postCapability(t, server.URL+asyncVideoSendURLPath, asyncVideoSendURLRequest{
		asyncBoundRequest: bound, URL: "https://cdn.example/video.mp4",
		Title: "sample", InvocationID: "call-url-video-1",
	})
	if status != http.StatusAccepted || body["job_id"] == "" {
		t.Fatalf("send URL status=%d body=%#v", status, body)
	}
	result := waitAsyncVideoJob(t, server.URL, bound, body["job_id"].(string))
	if result["queued"] != true || delivery.direct.InvocationID != "call-url-video-1" {
		t.Fatalf("result=%#v commit=%#v", result, delivery.direct)
	}
	if videos.inspectURL != "https://api.example/video" ||
		videos.inspectScope.RunID != "async:"+delivery.ticket.TicketHash ||
		videos.resolvedURL.URL != "https://cdn.example/video.mp4" || videos.resolvedURL.Title != "sample" {
		t.Fatalf("inspect=%#v url=%s resolved=%#v", videos.inspectScope, videos.inspectURL, videos.resolvedURL)
	}
}

func (*asyncVideoCapability) Release(VideoScope) {}

func TestAsyncVideoSearchAndSendUseTicketBoundDirectOutput(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	delivery := asyncStickerCapability(plainTicket)
	videos := new(asyncVideoCapability)
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: delivery, Videos: videos,
		AsyncDeliveryWake: func() { delivery.wakeCount++ },
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	gateway.runtimeCtx = context.Background()
	server := newAsyncVideoServer(gateway)
	defer server.Close()
	bound := asyncBoundForTicket(plainTicket, delivery.ticket)

	status, body := postCapability(t, server.URL+asyncVideoSearchPath, asyncVideoSearchRequest{
		asyncBoundRequest: bound, Category: "funny", Limit: 1,
	})
	if status != http.StatusOK || body["candidates"] == nil {
		t.Fatalf("search status=%d body=%#v", status, body)
	}
	status, body = postCapability(t, server.URL+asyncVideoSendPath, asyncVideoSendRequest{
		asyncBoundRequest: bound, CandidateID: "video-candidate", InvocationID: "call-video-1",
	})
	if status != http.StatusAccepted || body["job_id"] == "" {
		t.Fatalf("send status=%d body=%#v", status, body)
	}
	result := waitAsyncVideoJob(t, server.URL, bound, body["job_id"].(string))
	if result["queued"] != true || result["outbox_id"] != "outbox-direct" {
		t.Fatalf("status body=%#v", result)
	}
	if delivery.direct.Output.Kind != "video" || delivery.direct.InvocationID != "call-video-1" ||
		delivery.direct.ChatID != delivery.ticket.ChatID || delivery.wakeCount != 1 {
		t.Fatalf("commit=%#v wake=%d", delivery.direct, delivery.wakeCount)
	}
	if videos.searchScope.RunID != "async:"+delivery.ticket.TicketHash ||
		videos.selectScope.ChatID != delivery.ticket.ChatID {
		t.Fatalf("search=%#v select=%#v", videos.searchScope, videos.selectScope)
	}
}

func TestAsyncVideoRecoveryProcessesPersistedJobAfterReload(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	delivery := asyncStickerCapability(plainTicket)
	videos := new(asyncVideoCapability)
	jobs, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("open video job store: %v", err)
	}
	defer jobs.Close()
	job := domain.AsyncVideoJob{
		ID: "avjob_recovered", TicketHash: delivery.ticket.TicketHash,
		CandidateID: "video-candidate", InvocationID: "call-after-reload",
		CreatedAt: time.Now(),
	}
	if _, inserted, err := jobs.CreateAsyncVideoJob(
		context.Background(), job,
	); err != nil || !inserted {
		t.Fatalf("persist job inserted=%v err=%v", inserted, err)
	}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: delivery,
		Videos: videos, AsyncVideoJobs: jobs,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}

	gateway.dispatchRunnableAsyncVideoJobs(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, getErr := jobs.GetAsyncVideoJob(context.Background(), job.ID)
		if getErr == nil && stored.State == domain.AsyncVideoJobWaitingDelivery {
			if stored.Result.OutboxID != "outbox-direct" ||
				delivery.direct.InvocationID != job.InvocationID {
				t.Fatalf("stored=%#v commit=%#v", stored, delivery.direct)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("persisted async video job was not recovered")
}

func TestAsyncVideoJobRetriesDeadlineButFailsPermanentError(t *testing.T) {
	jobs, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("open video job store: %v", err)
	}
	defer jobs.Close()
	gateway, err := NewRelayGateway(RelayConfig{AsyncVideoJobs: jobs})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}

	retryJob := domain.AsyncVideoJob{
		ID: "avjob_retry", TicketHash: "ticket-retry", CandidateID: "candidate",
		InvocationID: "call-retry", CreatedAt: time.Now(),
	}
	_, _, _ = jobs.CreateAsyncVideoJob(context.Background(), retryJob)
	leased, err := jobs.LeaseAsyncVideoJob(
		context.Background(), retryJob.ID, time.Now(), time.Minute,
	)
	if err != nil {
		t.Fatalf("lease retry job: %v", err)
	}
	gateway.finishAsyncVideoJob(
		leased, domain.AsyncDirectOutputResult{}, context.DeadlineExceeded, false,
	)
	retried, err := jobs.GetAsyncVideoJob(context.Background(), retryJob.ID)
	if err != nil || retried.State != domain.AsyncVideoJobPending ||
		retried.Stage != "retry_wait" || retried.Attempt != 1 {
		t.Fatalf("retried job=%#v err=%v", retried, err)
	}

	failedJob := domain.AsyncVideoJob{
		ID: "avjob_permanent", TicketHash: "ticket-permanent", CandidateID: "candidate",
		InvocationID: "call-permanent", CreatedAt: time.Now(),
	}
	_, _, _ = jobs.CreateAsyncVideoJob(context.Background(), failedJob)
	leased, err = jobs.LeaseAsyncVideoJob(
		context.Background(), failedJob.ID, time.Now(), time.Minute,
	)
	if err != nil {
		t.Fatalf("lease permanent job: %v", err)
	}
	gateway.finishAsyncVideoJob(
		leased, domain.AsyncDirectOutputResult{}, fmt.Errorf("unsupported video container"), false,
	)
	failed, err := jobs.GetAsyncVideoJob(context.Background(), failedJob.ID)
	if err != nil || failed.State != domain.AsyncVideoJobFailed || failed.Attempt != 1 {
		t.Fatalf("failed job=%#v err=%v", failed, err)
	}
}

func TestAsyncVideoProgressUsesStableDirectOutput(t *testing.T) {
	plainTicket := "adt_1234567890123456789012345678901234567890"
	delivery := asyncStickerCapability(plainTicket)
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken:   testCapabilityToken,
		AsyncDelivery:     delivery,
		AsyncDeliveryWake: func() { delivery.wakeCount++ },
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	job := domain.AsyncVideoJob{
		ID: "avjob_1234567890abcdef", TicketHash: delivery.ticket.TicketHash,
		InvocationID: "call-progress", State: domain.AsyncVideoJobRunning,
		Stage: "preparing",
	}
	if err := gateway.commitAsyncVideoProgress(
		context.Background(), job, delivery.ticket,
	); err != nil {
		t.Fatalf("commitAsyncVideoProgress: %v", err)
	}
	if delivery.direct.InvocationID != job.ID+":progress:1" ||
		delivery.direct.Output.Kind != "text" || delivery.wakeCount != 1 {
		t.Fatalf("commit=%#v wake=%d", delivery.direct, delivery.wakeCount)
	}
	var output domain.TextOutput
	if err := json.Unmarshal(delivery.direct.Output.Payload, &output); err != nil {
		t.Fatalf("decode progress: %v", err)
	}
	if !strings.Contains(output.Content, "下载或处理视频") ||
		!strings.Contains(output.Content, "90abcdef") {
		t.Fatalf("progress content=%q", output.Content)
	}
}

func newAsyncVideoServer(gateway *RelayGateway) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(asyncVideoSearchPath, gateway.serveAsyncVideoSearch)
	mux.HandleFunc(asyncVideoInspectPath, gateway.serveAsyncVideoInspect)
	mux.HandleFunc(asyncVideoSendPath, gateway.serveAsyncVideoSend)
	mux.HandleFunc(asyncVideoSendURLPath, gateway.serveAsyncVideoSendURL)
	mux.HandleFunc(asyncVideoStatusPath, gateway.serveAsyncVideoStatus)
	return httptest.NewServer(mux)
}

func waitAsyncVideoJob(
	t *testing.T,
	baseURL string,
	bound asyncBoundRequest,
	jobID string,
) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, body := postCapability(t, baseURL+asyncVideoStatusPath, asyncVideoStatusRequest{
			asyncBoundRequest: bound, JobID: jobID,
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%#v", status, body)
		}
		if body["state"] != "pending" {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("async video job did not complete")
	return nil
}
