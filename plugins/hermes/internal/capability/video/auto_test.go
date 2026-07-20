package video

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAutoModeHandlesJSONErrorAndBinarySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("category") == "error" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"code":500,"message":"source unavailable"}`))
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write(append([]byte{0, 0, 0, 24}, []byte("ftypisom-video")...))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.ResponseMode = "auto"
	config.MaterializationMode = "on_select"
	config.Query = map[string]string{"category": "${category}"}
	config.Response.SuccessPath = "code"
	config.Response.SuccessValues = []string{"200"}
	config.Response.ErrorPath = "message"
	provider := newTestProvider(t, config)
	errorCandidates, _ := provider.Discover(context.Background(), DiscoveryRequest{Category: "error"})
	_, err := provider.Resolve(context.Background(), errorCandidates[0])
	var businessError *BusinessError
	if !errors.As(err, &businessError) {
		t.Fatalf("expected business error, got %v", err)
	}
	videoCandidates, _ := provider.Discover(context.Background(), DiscoveryRequest{Category: "beauty"})
	resolved, err := provider.Resolve(context.Background(), videoCandidates[0])
	if err != nil || resolved.Source.Body == nil {
		t.Fatalf("resolved=%#v error=%v", resolved, err)
	}
	_ = resolved.Source.Close()
}
