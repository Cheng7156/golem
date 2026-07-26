package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func TestImageCapabilityDescriptorExplainsLazyToolFlow(t *testing.T) {
	descriptor := relayDescriptor(relayDescriptorOptions{images: true})
	hint, _ := descriptor["platform_hint"].(string)
	for _, value := range []string{
		"never sent to vision automatically",
		"golem_image_inspect_current_session",
		"golem_image_search_current_session",
		"golem_image_read_current_session",
		"image, emoji, and sticker",
		"never collects or persists",
	} {
		if !strings.Contains(hint, value) {
			t.Fatalf("platform_hint missing %q: %s", value, hint)
		}
	}
}

func TestRelayDescriptorMakesCurrentDisplayNameAuthoritative(t *testing.T) {
	descriptor := relayDescriptor(relayDescriptorOptions{observationV2: true})
	hint, _ := descriptor["platform_hint"].(string)
	for _, value := range []string{
		"display_name",
		"current speaker",
		"nickname inferred from message text, older turns, or other participants",
	} {
		if !strings.Contains(hint, value) {
			t.Fatalf("platform_hint missing %q: %s", value, hint)
		}
	}
}

func TestImageCapabilityRequiresTokenAndReservesItsPaths(t *testing.T) {
	contextReader := &fakeInboundImageContext{}
	resolver := &fakeInboundImageResolver{}
	if _, err := NewRelayGateway(RelayConfig{ImageContext: contextReader, ImageResolver: resolver}); err == nil {
		t.Fatal("image capability started without a token")
	}
	for _, path := range []string{imageSearchPath, imageReadPath} {
		if _, err := NewRelayGateway(RelayConfig{
			Path: path, CapabilityToken: testCapabilityToken,
			ImageContext: contextReader, ImageResolver: resolver,
		}); err == nil {
			t.Fatalf("image capability accepted relay path collision %q", path)
		}
	}
}

type fakeInboundImageContext struct {
	values []domain.ContextMessage
	calls  int
	err    error
}

func (f *fakeInboundImageContext) ListRecentInboundContext(
	_ context.Context, _ string, _ int64, _ int,
) ([]domain.ContextMessage, error) {
	f.calls++
	return f.values, f.err
}

type fakeInboundImageResolver struct {
	mu    sync.Mutex
	calls int
	last  []domain.InboundMedia
	data  []byte
	mime  string
	err   error
}

type blockingInboundImageResolver struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	data    []byte
	mime    string
}

func (f *blockingInboundImageResolver) Resolve(ctx context.Context, media []domain.InboundMedia) ([]domain.InboundMedia, error) {
	f.mu.Lock()
	f.calls++
	if f.calls == 1 && f.started != nil {
		close(f.started)
	}
	f.mu.Unlock()
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result := append([]domain.InboundMedia(nil), media...)
	for index := range result {
		result[index].Data = append([]byte(nil), f.data...)
		result[index].MIMEType = f.mime
	}
	return result, nil
}

func (f *blockingInboundImageResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeInboundImageResolver) Resolve(
	_ context.Context, media []domain.InboundMedia,
) ([]domain.InboundMedia, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = append([]domain.InboundMedia(nil), media...)
	if f.err != nil {
		return nil, f.err
	}
	result := append([]domain.InboundMedia(nil), media...)
	for index := range result {
		result[index].Data = append([]byte(nil), f.data...)
		result[index].MIMEType = f.mime
	}
	return result, nil
}

func newImageCapabilityRun(t *testing.T, contextReader *fakeInboundImageContext, resolver *fakeInboundImageResolver) (*RelayGateway, *relayRun, *httptest.Server) {
	t.Helper()
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken,
		ImageContext:    contextReader,
		ImageResolver:   resolver,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := RunRequest{
		RunID: "run-image", SessionID: "chatroom:image-room", Lane: domain.LaneInteractive,
		Principal: domain.Principal{ID: "owner", Name: "Owner", IsOwner: true},
		Input:     "请处理", ChatType: "group", MessageID: "event-current", PlatformMessageID: "wx-current",
		CurrentEventID: "event-current", CurrentAcceptSeq: 20,
		CurrentMessage: domain.InboundMessage{
			SpeakerID: "owner", SpeakerName: "Owner", OccurredAt: time.Date(2026, 7, 24, 12, 2, 0, 0, time.UTC),
			Media: []domain.InboundMedia{{Kind: "image", MIMEType: "image/png", DownloadSource: []byte("current-source")}},
		},
		TriggerKind: domain.TriggerExplicit,
	}
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request), events: make(chan Event, 8),
		imageCandidates: make(map[string]relayImageCandidate), imageBySource: make(map[string]string),
	}
	gateway.pending[run.chatID] = run
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case imageSearchPath:
			gateway.serveImageSearch(w, r)
		case imageReadPath:
			gateway.serveImageRead(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	return gateway, run, server
}

func imageCapabilityContext(request RunRequest) capabilitySessionContext {
	chatID := relayChatID(request)
	return capabilitySessionContext{
		Platform: "relay", ChatID: chatID, UserID: request.Principal.ID,
		SessionKey: relaySessionKey(request, chatID), SessionID: request.SessionID,
		MessageID: request.MessageID,
	}
}

func postImageJSON(t *testing.T, target string, value any) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testCapabilityToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return response, data
}

func TestImageSearchIsLazyAndFiltersByVerifiedSender(t *testing.T) {
	contextReader := &fakeInboundImageContext{values: []domain.ContextMessage{
		{
			EventID: "event-old-member", PlatformMessageID: "wx-old-member", AcceptSeq: 17,
			OccurredAt: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
			Binding:    domain.ChannelBinding{Principal: domain.Principal{ID: "member", Name: "Miku"}},
			Message:    domain.InboundMessage{SpeakerID: "member", SpeakerName: "Miku", Media: []domain.InboundMedia{{Kind: "emoji", URL: "https://safe.invalid/sticker"}}},
		},
		{
			EventID: "event-old-other", PlatformMessageID: "wx-old-other", AcceptSeq: 18,
			OccurredAt: time.Date(2026, 7, 24, 12, 1, 0, 0, time.UTC),
			Binding:    domain.ChannelBinding{Principal: domain.Principal{ID: "other", Name: "Other"}},
			Message:    domain.InboundMessage{SpeakerID: "other", SpeakerName: "Other", Media: []domain.InboundMedia{{Kind: "image", URL: "https://safe.invalid/other"}}},
		},
	}}
	resolver := &fakeInboundImageResolver{data: []byte("unused"), mime: "image/png"}
	gateway, run, server := newImageCapabilityRun(t, contextReader, resolver)
	defer server.Close()
	defer gateway.removeRun(run)
	response, body := postImageJSON(t, server.URL+imageSearchPath, imageSearchRequest{
		SpeakerID: "member", Limit: 8, Context: imageCapabilityContext(run.request),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search status=%d body=%s", response.StatusCode, body)
	}
	var payload struct {
		Candidates []imageCandidateResponse `json:"candidates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(payload.Candidates) != 1 || payload.Candidates[0].SpeakerID != "member" || payload.Candidates[0].Kind != "emoji" {
		t.Fatalf("candidates=%#v", payload.Candidates)
	}
	if payload.Candidates[0].MessageID != "wx-old-member" || payload.Candidates[0].Readable != true {
		t.Fatalf("candidate identity=%#v", payload.Candidates[0])
	}
	if payload.Candidates[0].IsCurrentSender || payload.Candidates[0].IsCurrentMessage {
		t.Fatalf("historical candidate was marked current: %#v", payload.Candidates[0])
	}
	if resolver.calls != 0 || contextReader.calls != 1 {
		t.Fatalf("search performed materialization: resolver=%d context=%d", resolver.calls, contextReader.calls)
	}
}

func TestImageReadMaterializesOnlySelectedCandidateAndRejectsAmbient(t *testing.T) {
	contextReader := &fakeInboundImageContext{}
	resolver := &fakeInboundImageResolver{data: []byte("\x89PNG\r\n\x1a\n"), mime: "image/png"}
	gateway, run, server := newImageCapabilityRun(t, contextReader, resolver)
	defer server.Close()
	defer gateway.removeRun(run)
	searchResponse, searchBody := postImageJSON(t, server.URL+imageSearchPath, imageSearchRequest{
		Context: imageCapabilityContext(run.request), Limit: 1,
	})
	if searchResponse.StatusCode != http.StatusOK {
		t.Fatalf("search status=%d body=%s", searchResponse.StatusCode, searchBody)
	}
	var search struct {
		Candidates []imageCandidateResponse `json:"candidates"`
	}
	if err := json.Unmarshal(searchBody, &search); err != nil || len(search.Candidates) != 1 {
		t.Fatalf("search=%s err=%v", searchBody, err)
	}
	if !search.Candidates[0].IsCurrentSender || !search.Candidates[0].IsCurrentMessage {
		t.Fatalf("current candidate flags=%#v", search.Candidates[0])
	}
	candidateID := search.Candidates[0].ID
	readResponse, readBody := postImageJSON(t, server.URL+imageReadPath, imageReadRequest{
		CandidateID: candidateID, Question: "描述主体", Context: imageCapabilityContext(run.request),
	})
	if readResponse.StatusCode != http.StatusOK || string(readBody) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("read status=%d body=%q", readResponse.StatusCode, readBody)
	}
	if readResponse.Header.Get("Content-Type") != "image/png" || readResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("read headers=%v", readResponse.Header)
	}
	if resolver.calls != 1 || len(resolver.last) != 1 || string(resolver.last[0].DownloadSource) != "current-source" {
		t.Fatalf("resolver calls=%d last=%#v", resolver.calls, resolver.last)
	}
	cachedResponse, cachedBody := postImageJSON(t, server.URL+imageReadPath, imageReadRequest{
		CandidateID: candidateID, Context: imageCapabilityContext(run.request),
	})
	if cachedResponse.StatusCode != http.StatusOK || !bytes.Equal(cachedBody, readBody) {
		t.Fatalf("cached read status=%d body=%q", cachedResponse.StatusCode, cachedBody)
	}
	if resolver.calls != 1 {
		t.Fatalf("cached read re-materialized image: calls=%d", resolver.calls)
	}
	ambient := run.request
	ambient.TriggerKind = domain.TriggerAmbient
	run.request = ambient
	forbidden, _ := postImageJSON(t, server.URL+imageReadPath, imageReadRequest{
		CandidateID: candidateID, Context: imageCapabilityContext(run.request),
	})
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("ambient read status=%d", forbidden.StatusCode)
	}
}

func TestImageCandidatesKeepIdenticalMediaSeparatedByEventAndSender(t *testing.T) {
	shared := domain.InboundMedia{Kind: "emoji", MD5: "same-md5", URL: "https://safe.invalid/same"}
	contextReader := &fakeInboundImageContext{values: []domain.ContextMessage{
		{
			EventID: "event-a", PlatformMessageID: "wx-a", AcceptSeq: 18,
			Binding: domain.ChannelBinding{Principal: domain.Principal{ID: "sender-a", Name: "A"}},
			Message: domain.InboundMessage{SpeakerID: "sender-a", SpeakerName: "A", Media: []domain.InboundMedia{shared}},
		},
		{
			EventID: "event-b", PlatformMessageID: "wx-b", AcceptSeq: 19,
			Binding: domain.ChannelBinding{Principal: domain.Principal{ID: "sender-b", Name: "B"}},
			Message: domain.InboundMessage{SpeakerID: "sender-b", SpeakerName: "B", Media: []domain.InboundMedia{shared}},
		},
	}}
	resolver := &fakeInboundImageResolver{}
	gateway, run, server := newImageCapabilityRun(t, contextReader, resolver)
	defer server.Close()
	defer gateway.removeRun(run)
	candidates, err := gateway.searchRunImages(context.Background(), run, imageFilters{}, 8)
	if err != nil {
		t.Fatalf("searchRunImages: %v", err)
	}
	if len(candidates) < 3 {
		t.Fatalf("candidates=%#v", candidates)
	}
	bySpeaker := make(map[string]relayImageCandidate)
	for _, candidate := range candidates {
		bySpeaker[candidate.SpeakerID] = candidate
	}
	a, okA := bySpeaker["sender-a"]
	b, okB := bySpeaker["sender-b"]
	if !okA || !okB || a.ID == b.ID || a.SourceKey == b.SourceKey || a.EventID == b.EventID {
		t.Fatalf("identical media crossed sender/event identity: a=%#v b=%#v", a, b)
	}
}

func TestImageSearchRanksCurrentSenderBeforeNewerOtherSender(t *testing.T) {
	contextReader := &fakeInboundImageContext{values: []domain.ContextMessage{
		{
			EventID: "event-current-sender", PlatformMessageID: "wx-current-sender", AcceptSeq: 20,
			OccurredAt: time.Date(2026, 7, 24, 12, 1, 0, 0, time.UTC),
			Binding:    domain.ChannelBinding{Principal: domain.Principal{ID: "owner", Name: "Owner"}},
			Message: domain.InboundMessage{SpeakerID: "owner", SpeakerName: "Owner",
				Media: []domain.InboundMedia{{Kind: "image", URL: "https://safe.invalid/owner"}}},
		},
		{
			EventID: "event-newer-other", PlatformMessageID: "wx-newer-other", AcceptSeq: 30,
			OccurredAt: time.Date(2026, 7, 24, 12, 2, 0, 0, time.UTC),
			Binding:    domain.ChannelBinding{Principal: domain.Principal{ID: "other", Name: "Other"}},
			Message: domain.InboundMessage{SpeakerID: "other", SpeakerName: "Other",
				Media: []domain.InboundMedia{{Kind: "image", URL: "https://safe.invalid/other"}}},
		},
	}}
	resolver := &fakeInboundImageResolver{}
	gateway, run, server := newImageCapabilityRun(t, contextReader, resolver)
	defer server.Close()
	defer gateway.removeRun(run)
	run.request.CurrentMessage.Media = nil
	run.request.CurrentAcceptSeq = 40
	candidates, err := gateway.searchRunImages(context.Background(), run, imageFilters{}, 1)
	if err != nil {
		t.Fatalf("searchRunImages: %v", err)
	}
	if len(candidates) != 1 || candidates[0].SpeakerID != "owner" {
		t.Fatalf("unfiltered search did not prefer current sender: %#v", candidates)
	}

	// A candidate attached to the current event wins even when its durable
	// sequence is older than another participant's historical image.
	run.request.CurrentMessage = domain.InboundMessage{
		SpeakerID: "owner", SpeakerName: "Owner", OccurredAt: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		Media: []domain.InboundMedia{{Kind: "image", URL: "https://safe.invalid/current"}},
	}
	run.request.CurrentEventID = "event-current-now"
	run.request.CurrentAcceptSeq = 10
	candidates, err = gateway.searchRunImages(context.Background(), run, imageFilters{}, 1)
	if err != nil {
		t.Fatalf("current-message searchRunImages: %v", err)
	}
	if len(candidates) != 1 || candidates[0].EventID != "event-current-now" {
		t.Fatalf("current message was not ranked first: %#v", candidates)
	}
}

func TestImageCandidateIsRunScoped(t *testing.T) {
	contextReader := &fakeInboundImageContext{}
	resolver := &fakeInboundImageResolver{data: []byte("\x89PNG\r\n\x1a\n"), mime: "image/png"}
	gateway, run, server := newImageCapabilityRun(t, contextReader, resolver)
	defer server.Close()
	defer gateway.removeRun(run)
	response, body := postImageJSON(t, server.URL+imageSearchPath, imageSearchRequest{Context: imageCapabilityContext(run.request), Limit: 1})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search status=%d body=%s", response.StatusCode, body)
	}
	var search struct {
		Candidates []imageCandidateResponse `json:"candidates"`
	}
	if err := json.Unmarshal(body, &search); err != nil || len(search.Candidates) != 1 {
		t.Fatalf("search=%s err=%v", body, err)
	}
	delete(gateway.pending, run.chatID)
	stale, _ := postImageJSON(t, server.URL+imageReadPath, imageReadRequest{
		CandidateID: search.Candidates[0].ID, Context: imageCapabilityContext(run.request),
	})
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale read status=%d", stale.StatusCode)
	}
}
