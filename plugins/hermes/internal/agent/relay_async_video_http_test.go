package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type asyncVideoCapability struct {
	searchScope VideoScope
	selectScope VideoScope
}

func (f *asyncVideoCapability) Search(
	_ context.Context,
	scope VideoScope,
	_ VideoSearchInput,
) (VideoSearchResult, error) {
	f.searchScope = scope
	return VideoSearchResult{Candidates: []VideoCandidate{{
		ID: "video-candidate", ProviderID: "configured-provider", Title: "sample",
	}}}, nil
}

func (*asyncVideoCapability) ResolveURL(
	context.Context,
	VideoScope,
	VideoURLInput,
) (VideoCandidate, error) {
	return VideoCandidate{}, nil
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

func newAsyncVideoServer(gateway *RelayGateway) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(asyncVideoSearchPath, gateway.serveAsyncVideoSearch)
	mux.HandleFunc(asyncVideoSendPath, gateway.serveAsyncVideoSend)
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
