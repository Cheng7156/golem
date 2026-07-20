package video

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func testHTTPConfig(t *testing.T, server *httptest.Server) HTTPProviderConfig {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return HTTPProviderConfig{
		ProviderID:          "test_video",
		Categories:          []string{"general"},
		Endpoint:            server.URL,
		Method:              "GET",
		RequestMode:         "query",
		ResponseMode:        "json",
		MaterializationMode: "lazy",
		AllowedMediaHosts:   []string{parsed.Hostname()},
		AllowHTTP:           true,
		Response: ResponseMapping{
			URL: FieldMapping{Path: "url"},
		},
	}
}

func newTestProvider(t *testing.T, config HTTPProviderConfig, options ...HTTPProviderOption) Provider {
	t.Helper()
	provider, err := NewHTTPProvider(config, options...)
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	return provider
}
