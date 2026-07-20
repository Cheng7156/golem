package video

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONProviderDecodesArrayAndMappings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"data":{"items":[`+
			`{"play":"%s/one.mp4","title":" One ","duration":1500},`+
			`{"play":"%s/two.mp4","title":"Two","duration":2500}]}}`, serverURL(request), serverURL(request))
		_, _ = writer.Write([]byte(body))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.Response.ItemsPath = "data.items"
	config.Response.URL.Path = "play"
	config.Response.Title = FieldMapping{Path: "title", Transforms: []string{"trim"}}
	config.Response.Duration = FieldMapping{Path: "duration", Transforms: []string{"milliseconds_to_seconds"}}
	provider := newTestProvider(t, config)
	values, err := provider.Discover(context.Background(), DiscoveryRequest{Limit: 1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(values) != 1 || values[0].Title != "One" || values[0].DurationSeconds != 2 {
		t.Fatalf("candidates=%#v", values)
	}
}

func TestJSONProviderReturnsBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":429,"message":"quota exhausted"}`))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.Response.SuccessPath = "code"
	config.Response.SuccessValues = []string{"200"}
	config.Response.ErrorPath = "message"
	provider := newTestProvider(t, config)
	_, err := provider.Discover(context.Background(), DiscoveryRequest{})
	var businessError *BusinessError
	if !errors.As(err, &businessError) || businessError.Code != "429" {
		t.Fatalf("error=%v", err)
	}
}

func TestTextURLProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(writer, "  %s/video.mp4\n", serverURL(request))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.ResponseMode = "text_url"
	config.Response.URL.Transforms = []string{"trim"}
	provider := newTestProvider(t, config)
	values, err := provider.Discover(context.Background(), DiscoveryRequest{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	resolved, err := provider.Resolve(context.Background(), values[0])
	if err != nil || resolved.Source.URL == "" {
		t.Fatalf("resolved=%#v error=%v", resolved, err)
	}
}

func TestAutoModeRecognizesURLMislabeledAsOctetStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = fmt.Fprintf(writer, "%s/video.mp4", serverURL(request))
	}))
	defer server.Close()
	config := testHTTPConfig(t, server)
	config.ResponseMode = "auto"
	provider := newTestProvider(t, config)
	values, err := provider.Discover(context.Background(), DiscoveryRequest{})
	if err != nil || len(values) != 1 {
		t.Fatalf("values=%#v error=%v", values, err)
	}
	resolved, err := provider.Resolve(context.Background(), values[0])
	if err != nil || resolved.Source.URL == "" {
		t.Fatalf("resolved=%#v error=%v", resolved, err)
	}
}
