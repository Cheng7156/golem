package sticker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type deadlineDownloader struct{}

func (deadlineDownloader) Download(
	ctx context.Context,
	_ string,
	_ DownloadPolicy,
) ([]byte, string, error) {
	<-ctx.Done()
	return nil, "", ctx.Err()
}

func TestHTTPJSONMaterializeUsesProviderTimeout(t *testing.T) {
	config := apiHzConfig()
	config.Timeout = time.Millisecond
	provider, err := NewHTTPJSONProvider(
		config,
		WithProviderDownloader(deadlineDownloader{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Materialize(context.Background(), ProviderCandidate{
		Reference: "https://img.example.com/a.gif",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("materialize error=%v, want deadline exceeded", err)
	}
}
