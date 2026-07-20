package sticker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type materializeResult struct {
	output domain.EmojiOutput
	err    error
}

func TestMaterializeSingleFlightsAndReturnsImmutableCopies(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	provider := &fakeProvider{
		id: "remote", candidates: []ProviderCandidate{{Reference: "one"}},
		materialize: func(context.Context, ProviderCandidate) (domain.EmojiOutput, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return domain.EmojiOutput{Data: []byte("GIF89a"), MIMEType: "image/gif"}, nil
		},
	}
	service, scope, candidateID := newMaterializeFixture(t, provider, 64)
	results := make(chan materializeResult, 8)
	for range 8 {
		go func() {
			output, err := service.Materialize(context.Background(), scope, candidateID)
			results <- materializeResult{output: output, err: err}
		}()
	}
	<-started
	close(release)
	for range 8 {
		result := <-results
		if result.err != nil || string(result.output.Data) != "GIF89a" {
			t.Fatalf("materialize result=%#v", result)
		}
		result.output.Data[0] = 'X'
	}
	output, err := service.Materialize(context.Background(), scope, candidateID)
	if err != nil || string(output.Data) != "GIF89a" || calls.Load() != 1 {
		t.Fatalf("cached output=%#v err=%v calls=%d", output, err, calls.Load())
	}
}

func TestMaterializeFailureCanRetry(t *testing.T) {
	var calls atomic.Int32
	provider := &fakeProvider{
		id: "remote", candidates: []ProviderCandidate{{Reference: "one"}},
		materialize: func(context.Context, ProviderCandidate) (domain.EmojiOutput, error) {
			if calls.Add(1) == 1 {
				return domain.EmojiOutput{}, errors.New("temporary failure")
			}
			return domain.EmojiOutput{Data: []byte("GIF89a")}, nil
		},
	}
	service, scope, candidateID := newMaterializeFixture(t, provider, 64)
	if _, err := service.Materialize(context.Background(), scope, candidateID); err == nil {
		t.Fatal("expected first materialization to fail")
	}
	output, err := service.Materialize(context.Background(), scope, candidateID)
	if err != nil || string(output.Data) != "GIF89a" || calls.Load() != 2 {
		t.Fatalf("retry output=%#v err=%v calls=%d", output, err, calls.Load())
	}
}

func TestMaterializeBudgetEvictsWholeCandidate(t *testing.T) {
	var calls atomic.Int32
	provider := &fakeProvider{
		id:         "remote",
		candidates: []ProviderCandidate{{Reference: "one"}, {Reference: "two"}},
		materialize: func(context.Context, ProviderCandidate) (domain.EmojiOutput, error) {
			calls.Add(1)
			return domain.EmojiOutput{Data: []byte{0xff, 0xd8, 0xff, 0x00}}, nil
		},
	}
	service, scope, candidates := newMaterializeCandidates(t, provider, 4)
	if _, err := service.Materialize(context.Background(), scope, candidates[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Materialize(context.Background(), scope, candidates[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Materialize(context.Background(), scope, candidates[0].ID); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("evicted candidate error=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls=%d, want 2", calls.Load())
	}
}

func TestMaterializeRejectsOutputLargerThanBudget(t *testing.T) {
	provider := &fakeProvider{
		id: "remote", candidates: []ProviderCandidate{{Reference: "one"}},
		materialize: func(context.Context, ProviderCandidate) (domain.EmojiOutput, error) {
			return domain.EmojiOutput{Data: []byte{0xff, 0xd8, 0xff, 0x00, 0x00}}, nil
		},
	}
	service, scope, candidateID := newMaterializeFixture(t, provider, 4)
	_, err := service.Materialize(context.Background(), scope, candidateID)
	if !errors.Is(err, ErrMaterializedCacheFull) {
		t.Fatalf("oversized output error=%v", err)
	}
}

func newMaterializeFixture(
	t *testing.T,
	provider Provider,
	cacheBytes int64,
) (SearchService, Scope, string) {
	t.Helper()
	service, scope, candidates := newMaterializeCandidates(t, provider, cacheBytes)
	return service, scope, candidates[0].ID
}

func newMaterializeCandidates(
	t *testing.T,
	provider Provider,
	cacheBytes int64,
) (SearchService, Scope, []Candidate) {
	t.Helper()
	service, err := NewSearchService(ServiceConfig{
		CandidateTTL: 5 * time.Minute, MaxCandidates: 10,
		MaterializedCacheMaxBytes: cacheBytes,
	}, []Provider{provider})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RunID: "run-1", ChatID: "chat-1"}
	candidates, err := service.Search(context.Background(), SearchRequest{
		Scope: scope, Query: "reaction", Limit: 10,
	})
	if err != nil || len(candidates) == 0 {
		t.Fatalf("Search candidates=%#v err=%v", candidates, err)
	}
	return service, scope, candidates
}
