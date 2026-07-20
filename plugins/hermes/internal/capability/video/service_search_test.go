package video

import (
	"context"
	"errors"
	"testing"
)

func TestSearchReturnsCandidatesAndExposesPartialProviderFailure(t *testing.T) {
	failing := &serviceTestProvider{
		id: "failing", categories: []string{"funny"}, discoverError: errors.New("quota exhausted"),
	}
	working := &serviceTestProvider{id: "working", categories: []string{"funny"}}
	processor := serviceTestProcessor{directory: t.TempDir()}
	service := newServiceForTest(t, serviceTestConfig{
		providers: []RegisteredProvider{
			{Provider: failing, Priority: 10}, {Provider: working, Priority: 1},
		},
		processor: processor, maximum: 3,
	})
	result, err := service.Search(context.Background(), SearchRequest{
		Scope: Scope{RunID: "run", ChatID: "chat"}, Category: "funny", Limit: 2,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].ProviderID != "working" {
		t.Fatalf("candidates=%#v", result.Candidates)
	}
	if len(result.Failures) != 1 || result.Failures[0].ProviderID != "failing" {
		t.Fatalf("failures=%#v", result.Failures)
	}
}

func TestRegisterURLRejectsUnsafeScheme(t *testing.T) {
	processor := serviceTestProcessor{directory: t.TempDir()}
	service := newServiceForTest(t, serviceTestConfig{processor: processor, maximum: 1})
	_, err := service.RegisterURL(context.Background(), URLRequest{
		Scope: Scope{RunID: "run", ChatID: "chat"}, URL: "file:///etc/passwd",
	})
	if err == nil {
		t.Fatal("RegisterURL accepted a file URL")
	}
}
