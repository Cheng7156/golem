package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/capability/sticker"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/store/sqlite"
)

type recordingStickerService struct {
	search           sticker.SearchRequest
	bindScope        sticker.Scope
	bindProvider     string
	boundValues      []sticker.ProviderCandidate
	materializeScope sticker.Scope
	materializedID   string
}

func TestMapStickerMaterializeError(t *testing.T) {
	tests := []struct {
		input error
		want  error
	}{
		{sticker.ErrCandidateExpired, agent.ErrStickerCandidateUnavailable},
		{sticker.ErrCandidateScope, agent.ErrStickerCandidateUnavailable},
		{sticker.ErrMaterializedCacheFull, agent.ErrStickerMaterializeCacheFull},
		{context.DeadlineExceeded, agent.ErrStickerProviderTimeout},
		{errors.New("download failed"), agent.ErrStickerProviderUnavailable},
	}
	for _, test := range tests {
		if err := mapStickerMaterializeError(test.input); !errors.Is(err, test.want) {
			t.Fatalf("map error=%v, want %v", err, test.want)
		}
	}
}

func (s *recordingStickerService) Search(_ context.Context, request sticker.SearchRequest) ([]sticker.Candidate, error) {
	s.search = request
	return []sticker.Candidate{{ID: "opaque-1", Description: "开心"}}, nil
}

func (s *recordingStickerService) Bind(
	_ context.Context,
	scope sticker.Scope,
	providerID string,
	values []sticker.ProviderCandidate,
) ([]sticker.Candidate, error) {
	s.bindScope = scope
	s.bindProvider = providerID
	s.boundValues = append([]sticker.ProviderCandidate(nil), values...)
	result := make([]sticker.Candidate, 0, len(values))
	for index, value := range values {
		result = append(result, sticker.Candidate{
			ID: fmt.Sprintf("opaque-%d", index+1), Description: value.Description,
		})
	}
	return result, nil
}

func (s *recordingStickerService) Materialize(_ context.Context, scope sticker.Scope, candidateID string) (domain.EmojiOutput, error) {
	s.materializeScope = scope
	s.materializedID = candidateID
	return domain.EmojiOutput{Data: []byte("image")}, nil
}

func TestStickerCapabilityBridgeBoundsAgentInputAndPreservesScope(t *testing.T) {
	service := &recordingStickerService{}
	bridge := &stickerCapabilityBridge{
		service: service, expires: 5 * time.Minute, maxCandidates: 5, maxQueryRunes: 3,
		library: &sticker.LocalLibrary{},
	}
	scope := agent.StickerScope{RunID: "run-1", ChatID: "chat-1"}
	result, err := bridge.Search(context.Background(), scope, "开心大笑", 99)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if service.search.Query != "开心大" || service.search.Limit != 5 || service.search.Page != 1 {
		t.Fatalf("search request=%#v", service.search)
	}
	if service.search.Scope.RunID != scope.RunID || service.search.Scope.ChatID != scope.ChatID {
		t.Fatalf("search scope=%#v", service.search.Scope)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].ID != "opaque-1" || result.ExpiresInSeconds != 300 {
		t.Fatalf("search result=%#v", result)
	}
	if _, err := bridge.SearchLibrary(context.Background(), scope, "开心大笑", 2); err != nil {
		t.Fatalf("SearchLibrary: %v", err)
	}
	if service.search.Query != "开心大笑" || service.search.ProviderID != sticker.LocalLibraryProviderID || service.search.Limit != 2 {
		t.Fatalf("library search request=%#v", service.search)
	}

	if _, err := bridge.Select(context.Background(), scope, "opaque-1"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if service.materializeScope.RunID != scope.RunID || service.materializeScope.ChatID != scope.ChatID || service.materializedID != "opaque-1" {
		t.Fatalf("materialize scope=%#v id=%q", service.materializeScope, service.materializedID)
	}
}

func TestStickerCapabilityInventoryBindsCurrentRunCandidates(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 24)...)
	library, err := sticker.NewLocalLibrary(sticker.LibraryConfig{
		Directory: filepath.Join(t.TempDir(), "stickers"), MaxStorageBytes: 1024,
		MaxMediaBytes: int64(len(png)), CollectionPolicy: "owner",
	}, repository)
	if err != nil {
		t.Fatalf("NewLocalLibrary: %v", err)
	}
	collected, err := library.Collect(ctx, sticker.LibraryCollectRequest{
		Description: "群聊收藏", Data: png, MIMEType: "image/png",
		SourceSessionID: "chatroom:room-1", Collector: domain.Principal{ID: "owner", IsOwner: true},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	service := &recordingStickerService{}
	bridge := &stickerCapabilityBridge{
		service: service, library: library, expires: 5 * time.Minute,
	}
	scope := agent.StickerScope{RunID: "run-inventory", ChatID: "chat-inventory"}
	result, err := bridge.InventoryLibrary(ctx, scope, 20, 0)
	if err != nil {
		t.Fatalf("InventoryLibrary: %v", err)
	}
	if service.bindScope.RunID != scope.RunID || service.bindScope.ChatID != scope.ChatID ||
		service.bindProvider != sticker.LocalLibraryProviderID || len(service.boundValues) != 1 ||
		service.boundValues[0].Reference != collected.StickerID {
		t.Fatalf("inventory bind scope=%#v provider=%q values=%#v", service.bindScope, service.bindProvider, service.boundValues)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "opaque-1" ||
		result.Items[0].Description != "群聊收藏" || result.ExpiresInSeconds != 300 {
		t.Fatalf("inventory result=%#v", result)
	}
}
