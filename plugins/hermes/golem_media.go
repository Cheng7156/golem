package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/message"
	"google.golang.org/protobuf/proto"
)

const maxInboundMediaBytes = 16 << 20

type golemMediaResolver struct {
	message    message.Ability
	cdn        imageCDNDownloader
	httpClient *http.Client
}

type imageCDNDownloader interface {
	DownloadImage(fileID, fileAesKey string) (io.ReadCloser, error)
}

// imageCDNContextDownloader is implemented by the SDK gRPC wrapper and is
// intentionally optional so older host abilities remain source-compatible.
// A capability request must use this variant whenever it is available; the
// legacy method has no way to cancel a server-streaming RPC.
type imageCDNContextDownloader interface {
	DownloadImageContext(context.Context, string, string) (io.ReadCloser, error)
}

func (r golemMediaResolver) Resolve(
	ctx context.Context,
	media []domain.InboundMedia,
) ([]domain.InboundMedia, error) {
	resolved := append([]domain.InboundMedia(nil), media...)
	for index := range resolved {
		item := &resolved[index]
		if len(item.Data) > 0 {
			if len(item.Data) > maxInboundMediaBytes {
				return nil, errors.New("inbound WeChat media exceeds 16 MiB")
			}
			data, mimeType, err := normalizeInboundImage(item.Data)
			if err != nil {
				return nil, err
			}
			item.Data = data
			item.MIMEType = mimeType
			item.DownloadSource = nil
			continue
		}
		if len(item.DownloadSource) == 0 && strings.TrimSpace(item.URL) == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var source *message.Message
		if len(item.DownloadSource) > 0 {
			decoded := new(message.Message)
			if err := proto.Unmarshal(item.DownloadSource, decoded); err != nil {
				return nil, fmt.Errorf("decode persisted media source: %w", err)
			}
			source = decoded
		}
		reader, err := r.openMedia(ctx, item, source)
		if err != nil {
			if imageDownloadUnsupported(err) {
				// The lib-mode host cannot materialize inbound images. Keep the
				// structured [image] turn so Hermes can still observe or reply
				// based on verified identity and conversation context.
				item.DownloadSource = nil
				continue
			}
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxInboundMediaBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read WeChat media: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close WeChat media: %w", closeErr)
		}
		if len(data) == 0 {
			return nil, errors.New("downloaded WeChat media is empty")
		}
		if len(data) > maxInboundMediaBytes {
			return nil, errors.New("downloaded WeChat media exceeds 16 MiB")
		}
		data, mimeType, err := normalizeInboundImage(data)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item.Data = data
		item.MIMEType = mimeType
		item.DownloadSource = nil
	}
	return resolved, nil
}

func (r golemMediaResolver) openMedia(
	ctx context.Context,
	item *domain.InboundMedia,
	source *message.Message,
) (io.ReadCloser, error) {
	kind := strings.TrimSpace(item.Kind)
	var sourceMedia *message.Media
	recovered := recoveredWechatImage{}
	if source != nil {
		var sourceKind string
		sourceKind, sourceMedia = inboundMediaValue(source)
		if sourceKind != "" {
			kind = sourceKind
		}
		// Some host versions leave Media.{md5,key,url} empty for inbound
		// images, while the connector's raw JSON still contains the original
		// <imgmsg> attributes.  Recover those fields before choosing a CDN
		// identifier; otherwise a perfectly readable candidate is reported but
		// can never be materialized.
		recovered = recoverRawWechatImage(source.GetRaw())
	}
	if kind == "" {
		kind = "image"
	}
	fileID := inboundWechatFileID(item, sourceMedia, recovered)
	fileAesKey := inboundWechatAESKey(sourceMedia, recovered)
	directURL := trustedWechatMediaURL(item.URL)
	if directURL == "" && source != nil {
		directURL = firstTrustedWechatURL(recovered.URL, recovered.EncryptURL, recovered.ExternURL)
		if directURL == "" && sourceMedia != nil {
			directURL = trustedWechatMediaURL(sourceMedia.GetUrl())
		}
	}
	var directErr error
	// A signed, allowlisted URL is already a verified media reference.  Try it
	// first because the host CDN service can be slow or unavailable while the
	// URL remains directly reachable.  The response is still checked by
	// normalizeInboundImage before any bytes are exposed to Hermes.
	if directURL != "" {
		reader, err := r.downloadTrustedWechatURL(ctx, directURL)
		if err == nil && reader != nil {
			return reader, nil
		}
		if err == nil {
			err = errors.New("trusted WeChat URL downloader returned a nil reader")
		}
		directErr = fmt.Errorf("direct WeChat media fallback: %w", err)
	}

	var cdnErr error
	if r.cdn != nil && fileID != "" && fileAesKey != "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reader, err := r.downloadWechatCDN(ctx, fileID, fileAesKey)
		if err == nil && reader != nil {
			return reader, nil
		}
		if err == nil {
			err = errors.New("CDN downloader returned a nil reader")
		}
		cdnErr = fmt.Errorf("download WeChat CDN %s: %w", kind, err)
	}

	var messageErr error
	messageUnsupported := false
	// Keep the existing Message ability as a compatibility fallback for
	// ordinary images (not emojis, for which it has no download operation).
	if kind == "image" && r.message != nil && source != nil {
		reader, err := r.message.Download(source)
		if err == nil && reader != nil {
			return reader, nil
		}
		if err == nil {
			err = errors.New("message downloader returned a nil reader")
		}
		if rawImageDownloadUnsupported(err) {
			messageUnsupported = true
		} else {
			messageErr = err
		}
	}

	if directErr != nil || cdnErr != nil || messageErr != nil {
		return nil, errors.Join(directErr, cdnErr, messageErr)
	}
	if messageUnsupported {
		return nil, &unsupportedImageDownloadError{cause: errors.New("DownloadImg not supported in lib mode")}
	}
	if r.cdn == nil {
		return nil, errors.New("CDN media ability is unavailable")
	}
	return nil, errors.New("media is missing verified CDN identifiers")
}

// inboundWechatFileID returns the identifier accepted by the Golem CDN
// ability.  WeChat's normal image path uses the 32-character MD5 as the file
// id.  Older connector payloads may instead expose only cdnmidimgurl (an
// opaque serialized id), so retain that as a compatibility fallback.  Prefer
// a real MD5 whenever it is available, including one recovered from raw XML.
func inboundWechatFileID(
	item *domain.InboundMedia,
	sourceMedia *message.Media,
	recovered recoveredWechatImage,
) string {
	md5Values := []string{strings.TrimSpace(item.MD5)}
	if sourceMedia != nil {
		md5Values = append(md5Values, strings.TrimSpace(sourceMedia.GetMd5()))
	}
	md5Values = append(md5Values, strings.TrimSpace(recovered.MD5))
	for _, value := range md5Values {
		if isWechatMD5(value) {
			return value
		}
	}
	if sourceMedia != nil {
		if value := opaqueMediaFileID(sourceMedia.GetUrl()); value != "" {
			return value
		}
	}
	if value := opaqueMediaFileID(recovered.FileID); value != "" {
		return value
	}
	// Preserve non-standard but explicitly supplied ids for hosts that do not
	// use hexadecimal MD5s.  A URL is never accepted as a CDN id.
	for _, value := range md5Values {
		if value != "" {
			return value
		}
	}
	return ""
}

func inboundWechatAESKey(sourceMedia *message.Media, recovered recoveredWechatImage) string {
	if sourceMedia != nil {
		if value := strings.TrimSpace(sourceMedia.GetKey()); value != "" {
			return value
		}
	}
	return strings.TrimSpace(recovered.Key)
}

func isWechatMD5(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

const (
	// Direct WeChat URLs are short-lived signed references.  Keep their
	// individual deadline short so a failed edge does not consume the whole
	// Hermes capability request budget.
	wechatMediaHTTPTimeout = 5 * time.Second
	wechatMediaCDNTimeout  = 8 * time.Second
)

var defaultWechatMediaHTTPClient = &http.Client{
	Timeout: wechatMediaHTTPTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many WeChat media redirects")
		}
		if trustedWechatMediaURL(req.URL.String()) == "" {
			return errors.New("WeChat media redirect left the trusted domain allowlist")
		}
		return nil
	},
}

func (r golemMediaResolver) downloadWechatCDN(
	ctx context.Context,
	fileID string,
	fileAesKey string,
) (io.ReadCloser, error) {
	if contextual, ok := r.cdn.(imageCDNContextDownloader); ok {
		cdnCtx, cancel := context.WithTimeout(ctx, wechatMediaCDNTimeout)
		defer cancel()
		return contextual.DownloadImageContext(cdnCtx, fileID, fileAesKey)
	}

	// Older host implementations expose only DownloadImage, which cannot be
	// cancelled.  Bound the wait so the HTTP capability remains responsive; a
	// one-element result channel lets a late reader be closed without blocking
	// the worker goroutine.  Current SDK clients take the context-aware branch
	// above and do not leave a background RPC behind.
	type result struct {
		reader io.ReadCloser
		err    error
	}
	resultCh := make(chan result, 1)
	legacyCtx, cancel := context.WithTimeout(ctx, wechatMediaCDNTimeout)
	defer cancel()
	go func() {
		reader, err := r.cdn.DownloadImage(fileID, fileAesKey)
		select {
		case resultCh <- result{reader: reader, err: err}:
		case <-legacyCtx.Done():
			if reader != nil {
				_ = reader.Close()
			}
		}
	}()
	select {
	case value := <-resultCh:
		return value.reader, value.err
	case <-legacyCtx.Done():
		return nil, legacyCtx.Err()
	}
}

func (r golemMediaResolver) downloadTrustedWechatURL(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	trustedURL := trustedWechatMediaURL(rawURL)
	if trustedURL == "" {
		return nil, errors.New("WeChat media URL is not allowlisted")
	}
	requestCtx, cancel := context.WithTimeout(ctx, wechatMediaHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, trustedURL, nil)
	if err != nil {
		return nil, errors.New("invalid trusted WeChat media URL")
	}
	request.Header.Set("User-Agent", "golem-hermes-media/1")
	client := r.httpClient
	if client == nil {
		client = defaultWechatMediaHTTPClient
	}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// Do not put signed CDN query parameters into durable run errors/logs.
		return nil, errors.New("trusted WeChat media request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("trusted WeChat media returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxInboundMediaBytes {
		return nil, errors.New("trusted WeChat media exceeds 16 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxInboundMediaBytes+1))
	if err != nil {
		return nil, errors.New("reading trusted WeChat media failed")
	}
	if len(data) > maxInboundMediaBytes {
		return nil, errors.New("trusted WeChat media exceeds 16 MiB")
	}
	if _, err := detectInboundImageMIME(data); err != nil {
		return nil, errors.New("trusted WeChat media failed image validation")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type unsupportedImageDownloadError struct {
	cause error
}

func (e *unsupportedImageDownloadError) Error() string { return e.cause.Error() }
func (e *unsupportedImageDownloadError) Unwrap() error { return e.cause }

func imageDownloadUnsupported(err error) bool {
	var unsupported *unsupportedImageDownloadError
	return errors.As(err, &unsupported)
}

func rawImageDownloadUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "DownloadImg not supported in lib mode")
}

func opaqueMediaFileID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "://") {
		return ""
	}
	return value
}

func detectInboundImageMIME(data []byte) (string, error) {
	mimeType := http.DetectContentType(data)
	if !strings.HasPrefix(mimeType, "image/") || mimeType == "image/svg+xml" {
		return "", fmt.Errorf("WeChat media is not a supported image: %s", mimeType)
	}
	return mimeType, nil
}

const maxInboundGIFPixels = 16 << 20

func normalizeInboundImage(data []byte) ([]byte, string, error) {
	mimeType, err := detectInboundImageMIME(data)
	if err != nil {
		return nil, "", err
	}
	if mimeType != "image/gif" {
		return data, mimeType, nil
	}
	config, err := gif.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode WeChat GIF header: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxInboundGIFPixels {
		return nil, "", errors.New("WeChat GIF dimensions are too large")
	}
	frame, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode WeChat GIF: %w", err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		return nil, "", fmt.Errorf("encode WeChat GIF as PNG: %w", err)
	}
	if encoded.Len() > maxInboundMediaBytes {
		return nil, "", errors.New("converted WeChat GIF exceeds 16 MiB")
	}
	return encoded.Bytes(), "image/png", nil
}
