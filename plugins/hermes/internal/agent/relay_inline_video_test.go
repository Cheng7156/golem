package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/store/sqlite"
)

func TestInlineVideoFetchCreatesDurableJobWithoutBackgroundDelegation(t *testing.T) {
	ctx := context.Background()
	jobs, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	defer jobs.Close()
	delivery := new(recordingAsyncCapability)
	videos := new(asyncVideoCapability)
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: delivery,
		AsyncVideoJobs: jobs, Videos: videos,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	gateway.runtimeCtx = ctx
	runRequest := RunRequest{
		RunID: "run-inline-video", SessionID: "chatroom:room", MessageID: "event-inline",
		Principal: domain.Principal{ID: "wxid-owner"}, ChatType: "group",
	}
	chatID := relayChatID(runRequest)
	gateway.pending[chatID] = &relayRun{
		engine: gateway, request: runRequest, chatID: chatID,
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveInlineVideoFetch))
	defer server.Close()

	status, body := postCapability(t, server.URL, inlineVideoFetchRequest{
		URL: "https://api.example/video", InvocationID: "call-inline-video",
		ProducerEpoch: "ade_inline_epoch", HermesSessionID: "hermes-user-session",
		Context: capabilitySessionContext{
			Platform: "relay", ChatID: chatID, UserID: "wxid-owner",
			SessionKey: relaySessionKey(runRequest, chatID), SessionID: "hermes-user-session",
			MessageID: runRequest.MessageID, Profile: "default",
		},
	})
	if status != http.StatusAccepted || body["accepted"] != true || body["job_id"] == "" {
		t.Fatalf("status=%d body=%#v", status, body)
	}
	jobID := body["job_id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, getErr := jobs.GetAsyncVideoJob(ctx, jobID)
		if getErr == nil && job.State == domain.AsyncVideoJobWaitingDelivery {
			if !job.AutoClose || job.SourceURL != "https://api.example/video" ||
				delivery.direct.Output.Kind != "video" || !delivery.committed.Silent {
				t.Fatalf("job=%#v direct=%#v close=%#v", job, delivery.direct, delivery.committed)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("inline video job did not complete")
}

func TestAmbientInlineVideoFetchIsRejectedBeforeDurableRegistration(t *testing.T) {
	ctx := context.Background()
	jobs, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	defer jobs.Close()
	delivery := new(recordingAsyncCapability)
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, AsyncDelivery: delivery,
		AsyncVideoJobs: jobs, Videos: new(asyncVideoCapability),
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	runRequest := RunRequest{
		RunID: "run-ambient-inline-video", SessionID: "chatroom:room",
		MessageID: "event-ambient-inline", TriggerKind: domain.TriggerAmbient,
		Principal: domain.Principal{ID: "wxid-owner"}, ChatType: "group",
	}
	chatID := relayChatID(runRequest)
	gateway.pending[chatID] = &relayRun{
		engine: gateway, request: runRequest, chatID: chatID,
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveInlineVideoFetch))
	defer server.Close()

	status, body := postCapability(t, server.URL, inlineVideoFetchRequest{
		URL: "https://api.example/video", InvocationID: "call-ambient-inline",
		ProducerEpoch: "ade_inline_epoch", HermesSessionID: "hermes-user-session",
		Context: capabilitySessionContext{
			Platform: "relay", ChatID: chatID, UserID: "wxid-owner",
			SessionKey: relaySessionKey(runRequest, chatID), SessionID: "hermes-user-session",
			MessageID: runRequest.MessageID, Profile: "default",
		},
	})
	if status != http.StatusForbidden || body["error"] != ambientMediaDenied {
		t.Fatalf("status=%d body=%#v", status, body)
	}
	if delivery.registered.ParentRunID != "" || delivery.ticket.ID != "" {
		t.Fatalf("ambient request registered durable delivery: %#v", delivery)
	}
}

func TestAutomaticVideoInspectionRejectsAmbiguousJSON(t *testing.T) {
	_, err := automaticVideoInspectionURL(VideoURLInspection{
		Kind: "json",
		Candidates: []VideoURLReference{
			{URL: "https://cdn.example/one.mp4", Score: 45},
			{URL: "https://cdn.example/two.mp4", Score: 40},
		},
	})
	if err == nil {
		t.Fatal("ambiguous JSON inspection unexpectedly selected a URL")
	}
}
