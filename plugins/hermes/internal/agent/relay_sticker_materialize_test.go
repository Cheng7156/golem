package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type materializeHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func TestStickerMaterializeReturnsBytesWithoutStaging(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)
	response := postMaterialize(t, fixture.server.URL+stickerMaterializePath, stickerSelectRequest{
		CandidateID: "candidate-1", Context: capabilityContext(fixture.request),
	})
	if response.status != http.StatusOK || string(response.body) != "sticker-bytes" {
		t.Fatalf("materialize status=%d body=%q", response.status, response.body)
	}
	if response.header.Get("Content-Type") != "image/png" {
		t.Fatalf("content type=%q", response.header.Get("Content-Type"))
	}
	if response.header.Get("Cache-Control") != "no-store" || response.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers=%v", response.header)
	}
	fixture.sendFinal(t, "plain reply")
	want := []EventKind{EventRunAccepted, EventReplyProposed, EventRunCompleted}
	for _, kind := range want {
		if event := recvRelayEvent(t, fixture.stream); event.Kind != kind {
			t.Fatalf("event kind=%s, want %s", event.Kind, kind)
		}
	}
}

func TestStickerMaterializeHidesCandidateErrors(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)
	response := postMaterialize(t, fixture.server.URL+stickerMaterializePath, stickerSelectRequest{
		CandidateID: "candidate-does-not-exist", Context: capabilityContext(fixture.request),
	})
	if response.status != http.StatusNotFound {
		t.Fatalf("materialize status=%d body=%q", response.status, response.body)
	}
	if bytes.Contains(response.body, []byte("candidate-does-not-exist")) {
		t.Fatalf("response exposes candidate details: %q", response.body)
	}
}

func TestStickerMaterializeMapsSafeErrorStatuses(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{ErrStickerCandidateUnavailable, http.StatusNotFound},
		{ErrStickerMaterializeCacheFull, http.StatusServiceUnavailable},
		{ErrStickerProviderTimeout, http.StatusGatewayTimeout},
		{errors.New("provider detail"), http.StatusBadGateway},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		writeStickerMaterializeError(recorder, test.err)
		if recorder.Code != test.status || bytes.Contains(recorder.Body.Bytes(), []byte("provider detail")) {
			t.Fatalf("error=%v status=%d body=%q", test.err, recorder.Code, recorder.Body.String())
		}
	}
}

func postMaterialize(t *testing.T, target string, value any) materializeHTTPResponse {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testCapabilityToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return materializeHTTPResponse{status: response.StatusCode, header: response.Header, body: body}
}
