package sticker

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fixedResolver map[string][]net.IP

func (r fixedResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	values := r[host]
	result := make([]net.IPAddr, 0, len(values))
	for _, value := range values {
		result = append(result, net.IPAddr{IP: value})
	}
	return result, nil
}

func TestSecureDownloaderValidatesHostIPRedirectSizeAndMagic(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nrest")
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/ok":
			_, _ = writer.Write(png)
		case "/redirect":
			http.Redirect(writer, request, "https://evil.example/image.png", http.StatusFound)
		case "/large":
			_, _ = io.WriteString(writer, strings.Repeat("x", 32))
		case "/text":
			_, _ = io.WriteString(writer, "not an image")
		}
	}))
	defer server.Close()
	dialer := &net.Dialer{}
	downloader := NewSecureDownloader(
		WithDownloaderResolver(fixedResolver{
			"media.example":   {net.ParseIP("8.8.8.8")},
			"evil.example":    {net.ParseIP("1.1.1.1")},
			"private.example": {net.ParseIP("127.0.0.1")},
		}),
		WithDownloaderDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		}),
		WithDownloaderTLSConfig(&tls.Config{InsecureSkipVerify: true}), // test server certificate
	)

	data, mimeType, err := downloader.Download(context.Background(), "https://media.example/ok", DownloadPolicy{
		AllowedHosts: []string{"media.example"}, MaxBytes: 64,
	})
	if err != nil || string(data) != string(png) || mimeType != "image/png" {
		t.Fatalf("data=%q mime=%q err=%v", data, mimeType, err)
	}
	tests := []struct {
		name     string
		url      string
		policy   DownloadPolicy
		contains string
	}{
		{"requires https", "http://media.example/ok", DownloadPolicy{AllowedHosts: []string{"media.example"}}, "HTTPS"},
		{"allowlist", "https://other.example/ok", DownloadPolicy{AllowedHosts: []string{"media.example"}}, "allowlisted"},
		{"private IP", "https://private.example/ok", DownloadPolicy{AllowedHosts: []string{"private.example"}}, "non-public"},
		{"redirect", "https://media.example/redirect", DownloadPolicy{AllowedHosts: []string{"media.example"}}, "allowlisted"},
		{"size", "https://media.example/large", DownloadPolicy{AllowedHosts: []string{"media.example"}, MaxBytes: 8}, "size"},
		{"magic", "https://media.example/text", DownloadPolicy{AllowedHosts: []string{"media.example"}, MaxBytes: 64}, "supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := downloader.Download(context.Background(), test.url, test.policy)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want substring %q", err, test.contains)
			}
		})
	}
}

func TestImageMagic(t *testing.T) {
	tests := []struct {
		data []byte
		mime string
	}{
		{[]byte{0xff, 0xd8, 0xff, 0x00}, "image/jpeg"},
		{[]byte("\x89PNG\r\n\x1a\n"), "image/png"},
		{[]byte("GIF87a"), "image/gif"},
		{[]byte("GIF89a"), "image/gif"},
		{[]byte("RIFFxxxxWEBP"), "image/webp"},
		{[]byte("BMxxxx"), "image/bmp"},
	}
	for _, test := range tests {
		mimeType, err := imageMIME(test.data)
		if err != nil || mimeType != test.mime {
			t.Errorf("imageMIME(%q)=%q,%v want %q", test.data, mimeType, err, test.mime)
		}
	}
}
