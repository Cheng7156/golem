package video

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderErrorRedactsCredential(t *testing.T) {
	const secret = "top-secret-api-key"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte("rejected credential " + request.Header.Get("X-API-Key")))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.Headers = map[string]string{"X-API-Key": "${env:VIDEO_API_KEY}"}
	provider := newTestProvider(t, config, WithEnvironment(func(name string) (string, bool) {
		return secret, name == "VIDEO_API_KEY"
	}))
	_, err := provider.Discover(context.Background(), DiscoveryRequest{})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("credential was not redacted: %v", err)
	}
}

func TestProviderRejectsDisallowedMediaHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"url":"https://evil.example/video.mp4"}`))
	}))
	defer server.Close()
	provider := newTestProvider(t, testHTTPConfig(t, server))
	_, err := provider.Discover(context.Background(), DiscoveryRequest{})
	if err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("expected disallowed host error, got %v", err)
	}
}
