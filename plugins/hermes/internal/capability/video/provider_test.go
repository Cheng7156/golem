package video

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBinaryProviderDefersRequestUntilResolve(t *testing.T) {
	var calls atomic.Int32
	payload := append([]byte{0, 0, 0, 24}, []byte("ftypisom-video")...)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "video/mp4")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.ResponseMode = "binary"
	config.MaterializationMode = "on_select"
	provider := newTestProvider(t, config)
	candidates, err := provider.Discover(context.Background(), DiscoveryRequest{Category: "beauty"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calls.Load() != 0 || len(candidates) != 1 {
		t.Fatalf("calls=%d candidates=%d", calls.Load(), len(candidates))
	}
	resolved, err := provider.Resolve(context.Background(), candidates[0])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer resolved.Source.Close()
	actual, _ := io.ReadAll(resolved.Source.Body)
	if string(actual) != string(payload) || calls.Load() != 1 {
		t.Fatalf("payload=%q calls=%d", actual, calls.Load())
	}
}

func TestJSONRequestPreservesNativeTypesAndExpandsNestedTemplates(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"url":"` + serverURL(request) + `/video.mp4"}`))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.Method = http.MethodPost
	config.RequestMode = "json"
	config.JSONBody = map[string]any{
		"limit": int64(3), "enabled": true,
		"filters": map[string]any{"query": "${query}", "tags": []any{"video", "${category}"}},
	}
	provider := newTestProvider(t, config)
	_, err := provider.Discover(context.Background(), DiscoveryRequest{Query: "cats", Category: "funny"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	filters, _ := received["filters"].(map[string]any)
	if received["limit"] != float64(3) || received["enabled"] != true || filters["query"] != "cats" {
		t.Fatalf("received=%#v", received)
	}
	tags, _ := filters["tags"].([]any)
	if len(tags) != 2 || tags[1] != "funny" {
		t.Fatalf("tags=%#v", tags)
	}
}

func TestJSONProviderBuildsQueryAndMediaHeaders(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedQuery = request.URL.Query().Get("q")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"url":"` + serverURL(request) + `/media.mp4"}}`))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.Query = map[string]string{"q": "${query}"}
	config.MediaHeaders = map[string]string{"Referer": "${env:VIDEO_REFERER}"}
	config.Response.URL.Path = "data.url"
	provider := newTestProvider(t, config, WithEnvironment(func(name string) (string, bool) {
		return "https://page.example/video", name == "VIDEO_REFERER"
	}))
	candidates, err := provider.Discover(context.Background(), DiscoveryRequest{Query: "cats"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	resolved, err := provider.Resolve(context.Background(), candidates[0])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if receivedQuery != "cats" || resolved.Source.Headers.Get("Referer") != "https://page.example/video" {
		t.Fatalf("query=%q headers=%v", receivedQuery, resolved.Source.Headers)
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
