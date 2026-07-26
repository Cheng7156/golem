package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeStickerLibraryCapability struct {
	authorizeErr    error
	inventoryScope  StickerScope
	inventoryLimit  int
	inventoryOffset int
	inventoryResult StickerLibraryInventoryResult
	inventoryErr    error
	searchScope     StickerScope
	searchQuery     string
	searchLimit     int
	searchResult    StickerSearchResult
	searchErr       error
	collectScope    StickerScope
	collection      StickerLibraryCollection
	collectResult   StickerLibraryCollectionResult
	collectErr      error
}

func (f *fakeStickerLibraryCapability) InventoryLibrary(
	_ context.Context,
	scope StickerScope,
	limit int,
	offset int,
) (StickerLibraryInventoryResult, error) {
	f.inventoryScope, f.inventoryLimit, f.inventoryOffset = scope, limit, offset
	return f.inventoryResult, f.inventoryErr
}

func (f *fakeStickerLibraryCapability) AuthorizeCollection(
	_ context.Context,
	_ StickerScope,
) error {
	return f.authorizeErr
}

func (f *fakeStickerLibraryCapability) SearchLibrary(
	_ context.Context,
	scope StickerScope,
	query string,
	limit int,
) (StickerSearchResult, error) {
	f.searchScope, f.searchQuery, f.searchLimit = scope, query, limit
	return f.searchResult, f.searchErr
}

func (f *fakeStickerLibraryCapability) Collect(
	_ context.Context,
	scope StickerScope,
	request StickerLibraryCollection,
) (StickerLibraryCollectionResult, error) {
	f.collectScope, f.collection = scope, request
	return f.collectResult, f.collectErr
}

func TestStickerLibrarySearchAndCollectionUseRunScopedImageCandidate(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x21}, 16)...)
	contextReader := &fakeInboundImageContext{}
	resolver := &fakeInboundImageResolver{data: png, mime: "image/png"}
	gateway, run, oldServer := newImageCapabilityRun(t, contextReader, resolver)
	oldServer.Close()
	library := &fakeStickerLibraryCapability{
		inventoryResult: StickerLibraryInventoryResult{
			Items: []StickerLibraryInventoryItem{
				{Description: "群聊收藏"}, {Description: "私聊收藏"},
			},
			Total: 14, Limit: 20, Offset: 0, HasMore: false,
		},
		searchResult: StickerSearchResult{
			Candidates:       []StickerCandidate{{ID: "stc-local", Description: "大傻逼"}},
			ExpiresInSeconds: 300,
		},
		collectResult: StickerLibraryCollectionResult{
			Description: "大傻逼", AssetCreated: true, LabelCreated: true,
		},
	}
	gateway.config.Stickers = &fakeStickerCapability{}
	gateway.config.StickerLibrary = library
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case imageSearchPath:
			gateway.serveImageSearch(w, request)
		case stickerLibraryInventoryPath:
			gateway.serveStickerLibraryInventory(w, request)
		case stickerLibrarySearchPath:
			gateway.serveStickerLibrarySearch(w, request)
		case stickerLibraryCollectPath:
			gateway.serveStickerLibraryCollect(w, request)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	defer gateway.removeRun(run)

	status, inventory := postCapability(t, server.URL+stickerLibraryInventoryPath, stickerLibraryInventoryRequest{
		Limit: 20, Offset: 0, Context: imageCapabilityContext(run.request),
	})
	if status != http.StatusOK || inventory["total"] != float64(14) {
		t.Fatalf("library inventory status=%d body=%v", status, inventory)
	}
	if library.inventoryLimit != 20 || library.inventoryOffset != 0 ||
		library.inventoryScope.RunID != run.request.RunID {
		t.Fatalf("library inventory scope=%#v limit=%d offset=%d", library.inventoryScope, library.inventoryLimit, library.inventoryOffset)
	}

	status, search := postCapability(t, server.URL+stickerLibrarySearchPath, stickerSearchRequest{
		Query: "傻逼", Limit: 4, Context: imageCapabilityContext(run.request),
	})
	if status != http.StatusOK {
		t.Fatalf("library search status=%d body=%v", status, search)
	}
	if library.searchQuery != "傻逼" || library.searchLimit != 4 ||
		library.searchScope.RunID != run.request.RunID {
		t.Fatalf("library search scope=%#v query=%q limit=%d", library.searchScope, library.searchQuery, library.searchLimit)
	}

	response, body := postImageJSON(t, server.URL+imageSearchPath, imageSearchRequest{
		Limit: 1, Context: imageCapabilityContext(run.request),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("image search status=%d body=%s", response.StatusCode, body)
	}
	var images struct {
		Candidates []imageCandidateResponse `json:"candidates"`
	}
	if err := json.Unmarshal(body, &images); err != nil || len(images.Candidates) != 1 {
		t.Fatalf("image candidates=%#v err=%v", images.Candidates, err)
	}
	status, collected := postCapability(t, server.URL+stickerLibraryCollectPath, stickerLibraryCollectRequest{
		CandidateID: images.Candidates[0].ID, Description: "大傻逼",
		Context: imageCapabilityContext(run.request),
	})
	if status != http.StatusOK || collected["stored"] != true {
		t.Fatalf("collect status=%d body=%v", status, collected)
	}
	if !bytes.Equal(library.collection.Data, png) || library.collection.MIMEType != "image/png" {
		t.Fatalf("collection media=%#v", library.collection)
	}
	if library.collection.SourceEventID != run.request.CurrentEventID ||
		library.collection.SourceMessageID != run.request.PlatformMessageID ||
		library.collection.SourceSpeakerID != run.request.Principal.ID ||
		!library.collection.Collector.IsOwner {
		t.Fatalf("collection metadata=%#v", library.collection)
	}
	if resolver.calls != 1 {
		t.Fatalf("image resolver calls=%d, want 1", resolver.calls)
	}
}

func TestStickerLibraryPreviewStagesPageWithExactCounts(t *testing.T) {
	stickers := &fakeStickerCapability{}
	library := &fakeStickerLibraryCapability{
		inventoryResult: StickerLibraryInventoryResult{
			Items: []StickerLibraryInventoryItem{
				{ID: "candidate-1", Description: "一"},
				{ID: "candidate-2", Description: "二"},
				{ID: "candidate-3", Description: "三"},
			},
			Total: 7, Limit: 3, Offset: 2, HasMore: true,
		},
	}
	gateway, run, server := newStickerLibraryCapabilityRun(t, stickers, library)
	defer server.Close()
	defer gateway.removeRun(run)

	status, response := postCapability(t, server.URL+stickerLibraryPreviewPath, stickerLibraryPreviewRequest{
		Limit: 3, Offset: 2, Context: capabilityContext(run.request),
	})
	if status != http.StatusOK || response["staged"] != true ||
		response["staged_count"] != float64(3) || response["total"] != float64(7) ||
		response["next_offset"] != float64(5) || response["remaining_count"] != float64(2) {
		t.Fatalf("preview status=%d response=%#v", status, response)
	}
	run.mu.Lock()
	effectCount := len(run.effects)
	run.mu.Unlock()
	if effectCount != 3 {
		t.Fatalf("preview effects=%d, want 3", effectCount)
	}
	stickers.mu.Lock()
	defer stickers.mu.Unlock()
	if got := stickers.selectedIDs; len(got) != 3 || got[0] != "candidate-1" ||
		got[1] != "candidate-2" || got[2] != "candidate-3" {
		t.Fatalf("preview selected IDs=%v", got)
	}
}

func TestStickerLibraryPreviewStagesNothingWhenCandidateFails(t *testing.T) {
	library := &fakeStickerLibraryCapability{
		inventoryResult: StickerLibraryInventoryResult{
			Items: []StickerLibraryInventoryItem{
				{ID: "candidate-1", Description: "一"},
				{ID: "missing", Description: "失效"},
			},
			Total: 2, Limit: 2,
		},
	}
	gateway, run, server := newStickerLibraryCapabilityRun(t, &fakeStickerCapability{}, library)
	defer server.Close()
	defer gateway.removeRun(run)

	status, _ := postCapability(t, server.URL+stickerLibraryPreviewPath, stickerLibraryPreviewRequest{
		Limit: 2, Context: capabilityContext(run.request),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("preview status=%d, want %d", status, http.StatusBadRequest)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.effects) != 0 {
		t.Fatalf("failed preview staged %d effects", len(run.effects))
	}
}

func TestStickerLibraryPickSearchesAndStagesFirstMatch(t *testing.T) {
	stickers := &fakeStickerCapability{}
	library := &fakeStickerLibraryCapability{
		searchResult: StickerSearchResult{
			Candidates: []StickerCandidate{
				{ID: "candidate-2", Description: "闭嘴"},
				{ID: "candidate-1", Description: "安静"},
			},
		},
	}
	gateway, run, server := newStickerLibraryCapabilityRun(t, stickers, library)
	defer server.Close()
	defer gateway.removeRun(run)

	status, response := postCapability(t, server.URL+stickerLibraryPickPath, stickerSearchRequest{
		Query: "闭嘴", Limit: 5, Context: capabilityContext(run.request),
	})
	if status != http.StatusOK || response["staged"] != true ||
		response["match_count"] != float64(2) || response["description"] != "candidate-2" {
		t.Fatalf("pick status=%d response=%#v", status, response)
	}
	if library.searchQuery != "闭嘴" || library.searchLimit != 5 {
		t.Fatalf("pick query=%q limit=%d", library.searchQuery, library.searchLimit)
	}
	stickers.mu.Lock()
	defer stickers.mu.Unlock()
	if len(stickers.selectedIDs) != 1 || stickers.selectedIDs[0] != "candidate-2" {
		t.Fatalf("pick selected IDs=%v", stickers.selectedIDs)
	}
}

func TestStickerLibraryPickReturnsNoMatchWithoutEffect(t *testing.T) {
	gateway, run, server := newStickerLibraryCapabilityRun(
		t, &fakeStickerCapability{}, &fakeStickerLibraryCapability{},
	)
	defer server.Close()
	defer gateway.removeRun(run)

	status, response := postCapability(t, server.URL+stickerLibraryPickPath, stickerSearchRequest{
		Query: "不存在", Context: capabilityContext(run.request),
	})
	if status != http.StatusOK || response["staged"] != false || response["match_count"] != float64(0) {
		t.Fatalf("pick status=%d response=%#v", status, response)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.effects) != 0 {
		t.Fatalf("empty pick staged %d effects", len(run.effects))
	}
}

func newStickerLibraryCapabilityRun(
	t *testing.T,
	stickers StickerCapability,
	library StickerLibraryCapability,
) (*RelayGateway, *relayRun, *httptest.Server) {
	t.Helper()
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, Stickers: stickers, StickerLibrary: library,
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	request := stickerRunRequest("[direct]\nmessage: sticker library")
	run := &relayRun{
		engine: gateway, request: request, chatID: relayChatID(request), events: make(chan Event, 8),
	}
	gateway.pending[run.chatID] = run
	mux := http.NewServeMux()
	mux.HandleFunc(stickerLibraryPreviewPath, gateway.serveStickerLibraryPreview)
	mux.HandleFunc(stickerLibraryPickPath, gateway.serveStickerLibraryPick)
	return gateway, run, httptest.NewServer(mux)
}

func TestStickerLibraryRequiresTokenAndReservesPaths(t *testing.T) {
	library := &fakeStickerLibraryCapability{}
	if _, err := NewRelayGateway(RelayConfig{StickerLibrary: library}); err == nil {
		t.Fatal("sticker library started without a capability token")
	}
	if _, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, StickerLibrary: library,
	}); err == nil {
		t.Fatal("sticker library started without sticker selection capability")
	}
	for _, path := range []string{
		stickerLibraryInventoryPath, stickerLibrarySearchPath,
		stickerLibraryPreviewPath, stickerLibraryPickPath, stickerLibraryCollectPath,
	} {
		if _, err := NewRelayGateway(RelayConfig{
			Path: path, CapabilityToken: testCapabilityToken,
			Stickers: &fakeStickerCapability{}, StickerLibrary: library,
		}); err == nil {
			t.Fatalf("sticker library accepted relay path collision %q", path)
		}
	}
}

func TestStickerCollectionAuthorizationRunsBeforeImageMaterialization(t *testing.T) {
	resolver := &fakeInboundImageResolver{
		data: append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x31}, 8)...),
		mime: "image/png",
	}
	gateway, run, oldServer := newImageCapabilityRun(t, &fakeInboundImageContext{}, resolver)
	oldServer.Close()
	gateway.config.Stickers = &fakeStickerCapability{}
	gateway.config.StickerLibrary = &fakeStickerLibraryCapability{
		authorizeErr: ErrStickerCollectionForbidden,
	}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveStickerLibraryCollect))
	defer server.Close()
	defer gateway.removeRun(run)
	status, body := postCapability(t, server.URL, stickerLibraryCollectRequest{
		CandidateID: "img_not_materialized", Description: "不应下载",
		Context: imageCapabilityContext(run.request),
	})
	if status != http.StatusForbidden || body["error"] != "only the owner may collect stickers" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if resolver.calls != 0 {
		t.Fatalf("unauthorized collection resolved image %d times", resolver.calls)
	}
}
