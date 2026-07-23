package video

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestURLInspectorClassifiesDirectVideo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "video/mp4")
		_, _ = writer.Write([]byte("not-read-by-inspector"))
	}))
	defer server.Close()
	inspector := testURLInspector(t, server.Client())
	result, err := inspector.Inspect(context.Background(), server.URL+"/video")
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != "video" || result.FinalURL == "" {
		t.Fatalf("inspection=%#v", result)
	}
}

func TestURLInspectorSanitizesJSONAndRanksVideoURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"token":"https://secret.example/token","data":{"play_url":"https://cdn.example/video.mp4","title":"sample"}}`))
	}))
	defer server.Close()
	inspector := testURLInspector(t, server.Client())
	result, err := inspector.Inspect(context.Background(), server.URL+"/json")
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != "json" || len(result.Candidates) != 1 {
		t.Fatalf("inspection=%#v", result)
	}
	if result.Candidates[0].Path != "$.data.play_url" || result.Candidates[0].Score <= 10 {
		t.Fatalf("candidates=%#v", result.Candidates)
	}
	if result.Document.(map[string]any)["token"] != "[redacted]" {
		t.Fatalf("document=%#v", result.Document)
	}
}

func TestURLInspectorClassifiesTextAndJSONStringURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/json-string" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`"https://cdn.example/from-json.mp4"`))
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("https://cdn.example/from-text.mp4\n"))
	}))
	defer server.Close()
	inspector := testURLInspector(t, server.Client())
	for _, path := range []string{"/text", "/json-string"} {
		result, err := inspector.Inspect(context.Background(), server.URL+path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if result.Kind != "url" || len(result.Candidates) != 1 {
			t.Fatalf("%s inspection=%#v", path, result)
		}
	}
}

func TestURLInspectorRejectsPrivateURLBeforeRequest(t *testing.T) {
	inspector, err := NewURLInspector(URLInspectConfig{
		Timeout: time.Second, MaxBytes: 1 << 20, AllowHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inspector.Inspect(context.Background(), "http://127.0.0.1/video")
	if err == nil {
		t.Fatalf("err=%v", err)
	}
}

func TestURLInspectorRejectsUnsafeURLSyntaxBeforeRequest(t *testing.T) {
	inspector, err := NewURLInspector(URLInspectConfig{
		Timeout: time.Second, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rawURL := range []string{
		"http://example.com/video.mp4",
		"https://user:password@example.com/video.mp4",
		"file:///tmp/video.mp4",
	} {
		if _, inspectErr := inspector.Inspect(context.Background(), rawURL); inspectErr == nil {
			t.Fatalf("Inspect(%q) unexpectedly succeeded", rawURL)
		}
	}
}

func TestURLInspectorLimitsMetadataResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"response exceeds configured maximum"}`))
	}))
	defer server.Close()
	inspector, err := NewURLInspector(URLInspectConfig{
		Timeout: time.Second, MaxBytes: 16, AllowHTTP: true,
	}, WithURLInspectorHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, inspectErr := inspector.Inspect(context.Background(), server.URL); inspectErr == nil {
		t.Fatal("oversized response unexpectedly succeeded")
	}
}

func testURLInspector(t *testing.T, client *http.Client) *URLInspector {
	t.Helper()
	inspector, err := NewURLInspector(URLInspectConfig{
		Timeout: time.Second, MaxBytes: 1 << 20, AllowHTTP: true,
	}, WithURLInspectorHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	return inspector
}
