package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DownloadConfig struct {
	Directory string
	MaxBytes  int64
	Timeout   time.Duration
	AllowHTTP bool
}

type DownloadedMedia struct {
	Path        string
	Size        int64
	ContentType string
}

func (m DownloadedMedia) Remove() error {
	if strings.TrimSpace(m.Path) == "" {
		return nil
	}
	return os.Remove(m.Path)
}

type Downloader struct {
	config DownloadConfig
	client *http.Client
}

type DownloaderOption func(*Downloader)

func WithDownloadHTTPClient(client *http.Client) DownloaderOption {
	return func(downloader *Downloader) {
		if client != nil {
			downloader.client = client
		}
	}
}

func NewDownloader(config DownloadConfig, options ...DownloaderOption) (*Downloader, error) {
	config.Directory = filepath.Clean(strings.TrimSpace(config.Directory))
	if config.Directory == "." || config.MaxBytes <= 0 || config.Timeout <= 0 {
		return nil, errors.New("video downloader configuration is invalid")
	}
	if err := os.MkdirAll(config.Directory, 0o750); err != nil {
		return nil, fmt.Errorf("create video download directory: %w", err)
	}
	value := &Downloader{config: config, client: secureDownloadClient(config)}
	for _, option := range options {
		option(value)
	}
	return value, nil
}

func (d *Downloader) Download(ctx context.Context, source MediaSource) (DownloadedMedia, error) {
	if source.Body != nil {
		defer source.Close()
		return d.write(downloadWriteRequest{
			ctx: ctx, reader: source.Body, contentType: source.ContentType,
			contentLength: source.ContentLength,
		})
	}
	response, err := d.openURL(ctx, source)
	if err != nil {
		return DownloadedMedia{}, err
	}
	defer response.Body.Close()
	return d.write(downloadWriteRequest{
		ctx: ctx, reader: response.Body, contentType: response.Header.Get("Content-Type"),
		contentLength: response.ContentLength,
	})
}

func (d *Downloader) openURL(ctx context.Context, source MediaSource) (*http.Response, error) {
	parsed, err := url.Parse(strings.TrimSpace(source.URL))
	if err != nil || !validDownloadURL(parsed, d.config.AllowHTTP) {
		return nil, errors.New("video download URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header = make(http.Header)
	for name, values := range source.Headers {
		request.Header[name] = append([]string(nil), values...)
	}
	request.Header.Set("User-Agent", "golem-hermes-video/1.0")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download video: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return nil, fmt.Errorf("video server returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > d.config.MaxBytes {
		response.Body.Close()
		return nil, fmt.Errorf("video exceeds %d bytes", d.config.MaxBytes)
	}
	return response, nil
}

type downloadWriteRequest struct {
	ctx           context.Context
	reader        io.Reader
	contentType   string
	contentLength int64
}

func (d *Downloader) write(request downloadWriteRequest) (DownloadedMedia, error) {
	if request.contentLength > d.config.MaxBytes {
		return DownloadedMedia{}, fmt.Errorf("video exceeds %d bytes", d.config.MaxBytes)
	}
	file, err := os.CreateTemp(d.config.Directory, "download-*.media")
	if err != nil {
		return DownloadedMedia{}, fmt.Errorf("create video download file: %w", err)
	}
	path := file.Name()
	written, copyErr := copyWithContext(copyRequest{
		ctx: request.ctx, target: file, source: request.reader, maximum: d.config.MaxBytes,
	})
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written == 0 {
		_ = os.Remove(path)
		return DownloadedMedia{}, downloadWriteError(downloadWriteFailure{
			copyErr: copyErr, closeErr: closeErr,
		})
	}
	if err := validateDownloadedMedia(path, request.contentType); err != nil {
		_ = os.Remove(path)
		return DownloadedMedia{}, err
	}
	return DownloadedMedia{Path: path, Size: written, ContentType: request.contentType}, nil
}
