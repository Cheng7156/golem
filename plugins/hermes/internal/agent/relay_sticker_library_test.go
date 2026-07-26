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
	for _, path := range []string{stickerLibraryInventoryPath, stickerLibrarySearchPath, stickerLibraryCollectPath} {
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
