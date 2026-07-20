package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

func secureDownloadClient(config DownloadConfig) *http.Client {
	transport := &http.Transport{
		ForceAttemptHTTP2: true, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: config.Timeout,
		DialContext: dialPublicVideoAddress,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   config.Timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= maximumRedirects || !validDownloadURL(request.URL, config.AllowHTTP) {
				return errors.New("video redirect is not allowed")
			}
			if len(via) > 0 && !sameOrigin(request.URL, via[0].URL) {
				stripSensitiveHeaders(request.Header)
			}
			return nil
		},
	}
}

func validDownloadURL(value *url.URL, allowHTTP bool) bool {
	if value == nil || value.Hostname() == "" || value.User != nil {
		return false
	}
	return value.Scheme == "https" || (allowHTTP && value.Scheme == "http")
}

func dialPublicVideoAddress(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var failures []error
	for _, candidate := range addresses {
		if !publicVideoIP(candidate.IP) {
			continue
		}
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		failures = append(failures, dialErr)
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return nil, errors.New("video host did not resolve to a public address")
}

func publicVideoIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

type copyRequest struct {
	ctx     context.Context
	target  io.Writer
	source  io.Reader
	maximum int64
}

func copyWithContext(request copyRequest) (int64, error) {
	reader := &contextReader{ctx: request.ctx, source: io.LimitReader(request.source, request.maximum+1)}
	written, err := io.Copy(request.target, reader)
	if err != nil {
		return written, err
	}
	if written > request.maximum {
		return written, fmt.Errorf("video exceeds %d bytes", request.maximum)
	}
	return written, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

type downloadWriteFailure struct {
	copyErr  error
	closeErr error
}

func downloadWriteError(failure downloadWriteFailure) error {
	if failure.copyErr != nil {
		return fmt.Errorf("write video download: %w", failure.copyErr)
	}
	if failure.closeErr != nil {
		return fmt.Errorf("close video download: %w", failure.closeErr)
	}
	return errors.New("downloaded video is empty")
}
