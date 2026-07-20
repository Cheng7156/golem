package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/capability/sticker"
	"golem_plugin_hermes/internal/domain"
)

type recordingStickerService struct {
	search           sticker.SearchRequest
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

func (s *recordingStickerService) Materialize(_ context.Context, scope sticker.Scope, candidateID string) (domain.EmojiOutput, error) {
	s.materializeScope = scope
	s.materializedID = candidateID
	return domain.EmojiOutput{Data: []byte("image")}, nil
}

func TestStickerCapabilityBridgeBoundsAgentInputAndPreservesScope(t *testing.T) {
	service := &recordingStickerService{}
	bridge := &stickerCapabilityBridge{
		service: service, expires: 5 * time.Minute, maxCandidates: 5, maxQueryRunes: 3,
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

	if _, err := bridge.Select(context.Background(), scope, "opaque-1"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if service.materializeScope.RunID != scope.RunID || service.materializeScope.ChatID != scope.ChatID || service.materializedID != "opaque-1" {
		t.Fatalf("materialize scope=%#v id=%q", service.materializeScope, service.materializedID)
	}
}
