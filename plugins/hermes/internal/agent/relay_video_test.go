package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"

	"github.com/coder/websocket"
)

type fakeVideoCapability struct {
	selectError  error
	released     bool
	searchCalls  int
	resolveCalls int
	selectCalls  int
}

func (f *fakeVideoCapability) Search(
	context.Context,
	VideoScope,
	VideoSearchInput,
) (VideoSearchResult, error) {
	f.searchCalls++
	return VideoSearchResult{Candidates: []VideoCandidate{{
		ID: "video-candidate", ProviderID: "test", Title: "sample",
	}}}, nil
}

func (*fakeVideoCapability) InspectURL(
	context.Context,
	VideoScope,
	string,
) (VideoURLInspection, error) {
	return VideoURLInspection{Kind: "video", FinalURL: "https://example.com/video.mp4"}, nil
}

func (f *fakeVideoCapability) ResolveURL(
	_ context.Context,
	_ VideoScope,
	input VideoURLInput,
) (VideoCandidate, error) {
	f.resolveCalls++
	return VideoCandidate{ID: "url-candidate", ProviderID: "direct_url", PageURL: input.URL}, nil
}

func (f *fakeVideoCapability) Select(
	_ context.Context,
	_ VideoScope,
	candidateID string,
) (domain.VideoOutput, error) {
	f.selectCalls++
	if f.selectError != nil {
		return domain.VideoOutput{}, f.selectError
	}
	return domain.VideoOutput{
		ObjectID: "video-object", ThumbObjectID: "thumb-object", Duration: 2, Title: candidateID,
	}, nil
}

func (f *fakeVideoCapability) Release(VideoScope) {
	f.released = true
}

type videoTestFailure struct{}

func (videoTestFailure) Error() string    { return "transcode failed" }
func (videoTestFailure) Fallback() string { return "https://example.com/video" }

func TestRelayVideoJobStagesVideoEffect(t *testing.T) {
	fixture := newVideoRelayFixture(t, &fakeVideoCapability{}, true)
	defer fixture.close(t)
	status, response := fixture.startSelection(t)
	if status != http.StatusAccepted {
		t.Fatalf("select status=%d response=%v", status, response)
	}
	job := fixture.waitJob(t, response["job_id"].(string))
	if job["state"] != "completed" || job["staged"] != true || job["fallback"] != false {
		t.Fatalf("job=%v", job)
	}
	fixture.sendFinal(t, relayEffectOnlyToken)
	assertVideoEffectEvents(t, fixture.stream, "video")
}

func TestRelayVideoFailureStagesReasonAndLink(t *testing.T) {
	fixture := newVideoRelayFixture(t, &fakeVideoCapability{selectError: videoTestFailure{}}, true)
	defer fixture.close(t)
	_, response := fixture.startSelection(t)
	job := fixture.waitJob(t, response["job_id"].(string))
	if job["state"] != "completed" || job["fallback"] != true {
		t.Fatalf("job=%v", job)
	}
	fixture.sendFinal(t, relayEffectOnlyToken)
	effect := recvRelayEvent(t, fixture.stream)
	if effect.Proposal == nil || effect.Proposal.Kind != "text" ||
		!strings.Contains(string(effect.Proposal.Payload), "https://example.com/video") {
		t.Fatalf("effect=%#v", effect)
	}
	if completed := recvRelayEvent(t, fixture.stream); completed.Kind != EventRunCompleted {
		t.Fatalf("completed=%#v", completed)
	}
}

func TestRelayStagesMultipleVideoEffectsInSelectionOrder(t *testing.T) {
	fixture := newVideoRelayFixture(t, &fakeVideoCapability{}, true)
	defer fixture.close(t)
	first := fixture.startSelectionID(t, "candidate-first")
	second := fixture.startSelectionID(t, "candidate-second")
	fixture.waitJob(t, first["job_id"].(string))
	fixture.waitJob(t, second["job_id"].(string))
	fixture.sendFinal(t, relayEffectOnlyToken)
	for _, title := range []string{"candidate-first", "candidate-second"} {
		effect := recvRelayEvent(t, fixture.stream)
		var output domain.VideoOutput
		if effect.Proposal == nil || json.Unmarshal(effect.Proposal.Payload, &output) != nil || output.Title != title {
			t.Fatalf("effect=%#v output=%#v", effect, output)
		}
	}
	if completed := recvRelayEvent(t, fixture.stream); completed.Kind != EventRunCompleted {
		t.Fatalf("completed=%#v", completed)
	}
}

func TestRelayVideoDescriptorRequiresExplicitUserRequest(t *testing.T) {
	descriptor := relayDescriptor(relayDescriptorOptions{videos: true})
	hint, _ := descriptor["platform_hint"].(string)
	if !strings.Contains(hint, "only when the user explicitly asks") ||
		!strings.Contains(hint, "Never proactively send video") {
		t.Fatalf("platform_hint=%q", hint)
	}
}

func TestAmbientRunRejectsVideoCapabilitiesBeforeProviderOrJob(t *testing.T) {
	capability := &fakeVideoCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, Videos: capability,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := RunRequest{
		RunID: "run-ambient-video", SessionID: "chatroom:room-ambient-video",
		Lane: domain.LaneInteractive, TriggerKind: domain.TriggerAmbient,
		Principal: domain.Principal{ID: "wxid-owner"}, Input: "https://example.invalid/video",
		ChatType: "group", MessageID: "event-ambient-video",
	}
	chatID := relayChatID(request)
	gateway.pending[chatID] = &relayRun{
		engine: gateway, request: request, chatID: chatID, events: make(chan Event, 8),
	}
	server := newVideoRelayServer(gateway)
	defer server.Close()
	for _, test := range []struct {
		name string
		path string
		body any
	}{
		{name: "search", path: videoSearchPath, body: videoSearchRequest{
			Query: "sample", Limit: 1, Context: capabilityContext(request),
		}},
		{name: "resolve", path: videoResolvePath, body: videoResolveRequest{
			URL: "https://example.invalid/video", Context: capabilityContext(request),
		}},
		{name: "select", path: videoSelectPath, body: videoSelectRequest{
			CandidateID: "video-candidate", Context: capabilityContext(request),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, response := postCapability(t, server.URL+test.path, test.body)
			if status != http.StatusForbidden || response["error"] != ambientMediaDenied {
				t.Fatalf("status=%d response=%#v", status, response)
			}
		})
	}
	if capability.searchCalls != 0 || capability.resolveCalls != 0 || capability.selectCalls != 0 {
		t.Fatalf("ambient request reached video provider: %#v", capability)
	}
	gateway.videoMu.Lock()
	defer gateway.videoMu.Unlock()
	if len(gateway.videoJobs) != 0 {
		t.Fatalf("ambient request created video jobs: %#v", gateway.videoJobs)
	}
}

type videoRelayFixture struct {
	*stickerRelayFixture
	capability *fakeVideoCapability
}

func newVideoRelayFixture(
	t *testing.T,
	capability *fakeVideoCapability,
	fallback bool,
) *videoRelayFixture {
	t.Helper()
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, Videos: capability, VideoLinkFallback: fallback,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	server := newVideoRelayServer(gateway)
	base := connectRelayFixture(t, gateway, server)
	return &videoRelayFixture{stickerRelayFixture: base, capability: capability}
}

func newVideoRelayServer(gateway *RelayGateway) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(gateway.config.Path, gateway.serveRelay)
	mux.HandleFunc(videoSearchPath, gateway.serveVideoSearch)
	mux.HandleFunc(videoResolvePath, gateway.serveVideoResolve)
	mux.HandleFunc(videoSelectPath, gateway.serveVideoSelect)
	mux.HandleFunc(videoStatusPath, gateway.serveVideoStatus)
	return httptest.NewServer(mux)
}

func (f *videoRelayFixture) startSelection(t *testing.T) (int, map[string]any) {
	t.Helper()
	return f.startSelectionStatus(t, "video-candidate")
}

func (f *videoRelayFixture) startSelectionID(t *testing.T, candidateID string) map[string]any {
	t.Helper()
	_, result := f.startSelectionStatus(t, candidateID)
	return result
}

func (f *videoRelayFixture) startSelectionStatus(
	t *testing.T,
	candidateID string,
) (int, map[string]any) {
	t.Helper()
	return postCapability(t, f.server.URL+videoSelectPath, videoSelectRequest{
		CandidateID: candidateID, Context: capabilityContext(f.request),
	})
}

func (f *videoRelayFixture) waitJob(t *testing.T, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, result := postCapability(t, f.server.URL+videoStatusPath, videoStatusRequest{
			JobID: jobID, Context: capabilityContext(f.request),
		})
		if result["state"] != "pending" {
			return result
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("video job did not complete")
	return nil
}

func assertVideoEffectEvents(t *testing.T, stream Stream, kind string) {
	t.Helper()
	effect := recvRelayEvent(t, stream)
	if effect.Kind != EventEffectProposed || effect.Proposal == nil || effect.Proposal.Kind != kind {
		t.Fatalf("effect=%#v", effect)
	}
	completed := recvRelayEvent(t, stream)
	if completed.Kind != EventRunCompleted {
		t.Fatalf("completed=%#v", completed)
	}
}

func connectRelayFixture(
	t *testing.T,
	gateway *RelayGateway,
	server *httptest.Server,
) *stickerRelayFixture {
	t.Helper()
	connection, _, err := websocket.Dial(
		context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+gateway.config.Path, nil,
	)
	if err != nil {
		server.Close()
		t.Fatalf("Dial: %v", err)
	}
	writeRelayFrame(t, connection, map[string]any{
		"type": "hello", "platform": "relay", "botId": "golem",
	})
	_ = readRelayFrame(t, connection)
	request := stickerRunRequest("[direct] send a video")
	stream, err := gateway.Start(context.Background(), request)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "start failed")
		server.Close()
		t.Fatalf("Start: %v", err)
	}
	_ = readRelayFrame(t, connection)
	if accepted := recvRelayEvent(t, stream); accepted.Kind != EventRunAccepted {
		t.Fatalf("accepted=%#v", accepted)
	}
	return &stickerRelayFixture{
		server: server, connection: connection, stream: stream, request: request,
	}
}
