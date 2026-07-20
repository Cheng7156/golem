package video

import (
	"context"
	"errors"
	"testing"
)

func TestSelectSupportsMultipleOrderedVideosAndCachesPreparation(t *testing.T) {
	provider := &serviceTestProvider{id: "videos", categories: []string{"beauty"}}
	processor := serviceTestProcessor{directory: t.TempDir()}
	service := newServiceForTest(t, serviceTestConfig{
		providers: []RegisteredProvider{{Provider: provider}}, processor: processor, maximum: 2,
	})
	scope := Scope{RunID: "run", ChatID: "chat"}
	result, err := service.Search(context.Background(), SearchRequest{
		Scope: scope, Category: "beauty", Limit: 1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for index := 0; index < 2; index++ {
		output, selectErr := service.Select(context.Background(), scope, result.Candidates[0].ID)
		if selectErr != nil || output.ObjectID != "video-object" {
			t.Fatalf("Select %d: output=%#v error=%v", index, output, selectErr)
		}
	}
	if _, err := service.Select(context.Background(), scope, result.Candidates[0].ID); !errors.Is(err, ErrRunVideoLimit) {
		t.Fatalf("third Select error=%v", err)
	}
	if provider.resolveCalls.Load() != 1 {
		t.Fatalf("provider resolve calls=%d", provider.resolveCalls.Load())
	}
}

func TestSelectReturnsFallbackDetailsOnPreparationFailure(t *testing.T) {
	provider := &serviceTestProvider{
		id: "videos", categories: []string{"funny"}, fallbackURL: "https://example.com/page",
	}
	processor := serviceTestProcessor{directory: t.TempDir(), err: errTestPreparation}
	service := newServiceForTest(t, serviceTestConfig{
		providers: []RegisteredProvider{{Provider: provider}}, processor: processor, maximum: 1,
	})
	scope := Scope{RunID: "run", ChatID: "chat"}
	result, _ := service.Search(context.Background(), SearchRequest{Scope: scope, Category: "funny"})
	_, err := service.Select(context.Background(), scope, result.Candidates[0].ID)
	var preparation *PreparationError
	if !errors.As(err, &preparation) || preparation.FallbackURL != "https://example.com/page" {
		t.Fatalf("error=%v", err)
	}
}

func TestSelectRejectsCandidateFromAnotherChat(t *testing.T) {
	provider := &serviceTestProvider{id: "videos", categories: []string{"general"}}
	processor := serviceTestProcessor{directory: t.TempDir()}
	service := newServiceForTest(t, serviceTestConfig{
		providers: []RegisteredProvider{{Provider: provider}}, processor: processor, maximum: 1,
	})
	result, _ := service.Search(context.Background(), SearchRequest{
		Scope: Scope{RunID: "run", ChatID: "chat-a"}, Category: "general",
	})
	_, err := service.Select(
		context.Background(), Scope{RunID: "run", ChatID: "chat-b"}, result.Candidates[0].ID,
	)
	if !errors.Is(err, ErrCandidateScope) {
		t.Fatalf("Select error=%v", err)
	}
}
