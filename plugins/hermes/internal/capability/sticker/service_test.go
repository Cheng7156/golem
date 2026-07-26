package sticker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type fakeProvider struct {
	id          string
	candidates  []ProviderCandidate
	materialize func(context.Context, ProviderCandidate) (domain.EmojiOutput, error)
}

func (p *fakeProvider) ID() string { return p.id }

func (p *fakeProvider) Search(context.Context, ProviderSearchRequest) ([]ProviderCandidate, error) {
	return append([]ProviderCandidate(nil), p.candidates...), nil
}

func (p *fakeProvider) Materialize(ctx context.Context, candidate ProviderCandidate) (domain.EmojiOutput, error) {
	if p.materialize != nil {
		return p.materialize(ctx, candidate)
	}
	return domain.EmojiOutput{Data: []byte("image"), Description: candidate.Description}, nil
}

func TestSearchServiceOpaqueScopeBoundSelection(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	provider := &fakeProvider{
		id: "remote",
		candidates: []ProviderCandidate{{
			Reference:   "https://secret-source.example/sticker.gif",
			Description: "cat",
		}},
		materialize: func(_ context.Context, candidate ProviderCandidate) (domain.EmojiOutput, error) {
			if candidate.Reference != "https://secret-source.example/sticker.gif" {
				t.Fatalf("reference=%q", candidate.Reference)
			}
			return domain.EmojiOutput{
				URL: candidate.Reference, Data: []byte("GIF89a"),
				MIMEType: "image/gif", Description: candidate.Description,
			}, nil
		},
	}
	service, err := NewSearchService(ServiceConfig{
		CandidateTTL:  2 * time.Minute,
		MaxCandidates: 10,
	}, []Provider{provider}, WithServiceClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RunID: "run-1", ChatID: "chat-1"}
	candidates, err := service.Search(context.Background(), SearchRequest{
		Scope: scope, Query: "cat", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !strings.HasPrefix(candidates[0].ID, "stc_") {
		t.Fatalf("unexpected candidates: %#v", candidates)
	}
	if strings.Contains(candidates[0].ID, "secret-source") {
		t.Fatalf("candidate id exposes provider reference: %s", candidates[0].ID)
	}
	if _, err := service.Materialize(context.Background(), Scope{RunID: "run-2", ChatID: "chat-1"}, candidates[0].ID); !errors.Is(err, ErrCandidateScope) {
		t.Fatalf("cross-run error=%v", err)
	}
	if _, err := service.Materialize(context.Background(), Scope{RunID: "run-1", ChatID: "chat-2"}, candidates[0].ID); !errors.Is(err, ErrCandidateScope) {
		t.Fatalf("cross-chat error=%v", err)
	}
	output, err := service.Materialize(context.Background(), scope, candidates[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(output.Data) != "GIF89a" || output.URL != "" || output.MIMEType != "image/gif" || output.Description != "cat" {
		t.Fatalf("unexpected output: %#v", output)
	}
	now = now.Add(3 * time.Minute)
	if _, err := service.Materialize(context.Background(), scope, candidates[0].ID); !errors.Is(err, ErrCandidateExpired) {
		t.Fatalf("expired error=%v", err)
	}
}

func TestSearchServiceBindCreatesOpaqueScopeBoundCandidates(t *testing.T) {
	provider := &fakeProvider{
		id: "local",
		materialize: func(_ context.Context, candidate ProviderCandidate) (domain.EmojiOutput, error) {
			return domain.EmojiOutput{
				Data: []byte("GIF89a"), MIMEType: "image/gif", Description: candidate.Description,
			}, nil
		},
	}
	service, err := NewSearchService(ServiceConfig{
		CandidateTTL: 2 * time.Minute, MaxCandidates: 10,
	}, []Provider{provider})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RunID: "run-inventory", ChatID: "chat-inventory"}
	candidates, err := service.Bind(context.Background(), scope, provider.id, []ProviderCandidate{{
		Reference: "stable-library-id", Description: "群聊收藏",
	}})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(candidates) != 1 || !strings.HasPrefix(candidates[0].ID, "stc_") ||
		strings.Contains(candidates[0].ID, "stable-library-id") {
		t.Fatalf("bound candidates=%#v", candidates)
	}
	if _, err := service.Materialize(
		context.Background(), Scope{RunID: "other-run", ChatID: scope.ChatID}, candidates[0].ID,
	); !errors.Is(err, ErrCandidateScope) {
		t.Fatalf("cross-run error=%v", err)
	}
	if _, err := service.Materialize(
		context.Background(), Scope{RunID: scope.RunID, ChatID: "other-chat"}, candidates[0].ID,
	); !errors.Is(err, ErrCandidateScope) {
		t.Fatalf("cross-chat error=%v", err)
	}
	output, err := service.Materialize(context.Background(), scope, candidates[0].ID)
	if err != nil || output.Description != "群聊收藏" {
		t.Fatalf("Materialize=%#v err=%v", output, err)
	}
}
