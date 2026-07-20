package video

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloaderWritesDirectStream(t *testing.T) {
	downloader := newTestDownloader(t, 32)
	payload := append([]byte{0, 0, 0, 24}, []byte("ftypisom-video")...)
	result, err := downloader.Download(context.Background(), MediaSource{
		Body: io.NopCloser(bytes.NewReader(payload)), ContentType: "video/mp4", ContentLength: int64(len(payload)),
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer result.Remove()
	data, _ := os.ReadFile(result.Path)
	if !bytes.Equal(data, payload) || result.Size != int64(len(payload)) {
		t.Fatalf("result=%#v data=%q", result, data)
	}
}

func TestDownloaderRejectsHLSBeforeFFprobe(t *testing.T) {
	downloader := newTestDownloader(t, 128)
	_, err := downloader.Download(context.Background(), MediaSource{
		Body:        io.NopCloser(bytes.NewReader([]byte("#EXTM3U\nhttps://127.0.0.1/segment.ts"))),
		ContentType: "application/vnd.apple.mpegurl",
	})
	if err == nil {
		t.Fatal("Download accepted an HLS playlist")
	}
}

func TestDownloaderFetchesURLAndEnforcesLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("video-too-large"))
	}))
	defer server.Close()
	downloader := newTestDownloader(t, 5, WithDownloadHTTPClient(server.Client()))
	_, err := downloader.Download(context.Background(), MediaSource{URL: server.URL})
	if err == nil {
		t.Fatal("Download accepted an oversized response")
	}
}

func newTestDownloader(t *testing.T, maximum int64, options ...DownloaderOption) *Downloader {
	t.Helper()
	value, err := NewDownloader(DownloadConfig{
		Directory: filepath.Join(t.TempDir(), "downloads"), MaxBytes: maximum,
		Timeout: time.Second, AllowHTTP: true,
	}, options...)
	if err != nil {
		t.Fatalf("NewDownloader: %v", err)
	}
	return value
}
