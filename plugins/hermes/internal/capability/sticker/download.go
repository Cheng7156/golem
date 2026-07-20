package sticker

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const defaultMaximumStickerBytes int64 = 8 << 20

type DownloadPolicy struct {
	AllowedHosts []string
	MaxBytes     int64
}

type Downloader interface {
	Download(context.Context, string, DownloadPolicy) ([]byte, string, error)
}

type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type SecureDownloader struct {
	resolver  IPResolver
	dial      DialContextFunc
	tlsConfig *tls.Config
}

type downloadPolicyError struct{ message string }

func (e *downloadPolicyError) Error() string { return e.message }

func policyError(message string) error { return &downloadPolicyError{message: message} }

type DownloaderOption func(*SecureDownloader)

func WithDownloaderResolver(resolver IPResolver) DownloaderOption {
	return func(value *SecureDownloader) {
		if resolver != nil {
			value.resolver = resolver
		}
	}
}

func WithDownloaderDialer(dial DialContextFunc) DownloaderOption {
	return func(value *SecureDownloader) {
		if dial != nil {
			value.dial = dial
		}
	}
}

// WithDownloaderTLSConfig exists for custom trust roots. Production callers
// should never set InsecureSkipVerify; tests may use it with an httptest TLS
// server while still exercising host/IP policy.
func WithDownloaderTLSConfig(config *tls.Config) DownloaderOption {
	return func(value *SecureDownloader) {
		if config != nil {
			value.tlsConfig = config.Clone()
		}
	}
}

func NewSecureDownloader(options ...DownloaderOption) *SecureDownloader {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	value := &SecureDownloader{
		resolver: net.DefaultResolver,
		dial:     dialer.DialContext,
	}
	for _, option := range options {
		option(value)
	}
	return value
}

func (d *SecureDownloader) Download(
	ctx context.Context,
	rawURL string,
	policy DownloadPolicy,
) ([]byte, string, error) {
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = defaultMaximumStickerBytes
	}
	allowed, err := normalizeAllowedHosts(policy.AllowedHosts)
	if err != nil {
		return nil, "", err
	}
	parsed, err := validateMediaURL(rawURL, allowed)
	if err != nil {
		return nil, "", err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		TLSClientConfig:       cloneTLSConfig(d.tlsConfig),
	}
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, errors.New("invalid sticker media network address")
		}
		if !hostAllowed(host, allowed) {
			return nil, errors.New("sticker media host is not allowlisted")
		}
		addresses, resolveErr := resolvePublic(dialCtx, d.resolver, host)
		if resolveErr != nil {
			return nil, resolveErr
		}
		var failures []error
		for _, address := range addresses {
			connection, dialErr := d.dial(dialCtx, network, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			failures = append(failures, dialErr)
		}
		if len(failures) > 0 {
			return nil, errors.New("sticker media connection failed")
		}
		return nil, errors.New("sticker media host has no public address")
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many sticker media redirects")
			}
			_, redirectErr := validateMediaURL(request.URL.String(), allowed)
			return redirectErr
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", errors.New("create sticker media request")
	}
	request.Header.Set("User-Agent", "golem-hermes-sticker/1.0")
	request.Header.Set("Accept", "image/jpeg,image/png,image/gif,image/webp,image/bmp")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		var policyFailure *downloadPolicyError
		if errors.As(err, &policyFailure) {
			return nil, "", policyFailure
		}
		return nil, "", errors.New("download sticker media failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("sticker media returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > policy.MaxBytes {
		return nil, "", errors.New("sticker media exceeds configured size limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, policy.MaxBytes+1))
	if err != nil {
		return nil, "", errors.New("read sticker media failed")
	}
	if len(data) == 0 {
		return nil, "", errors.New("sticker media is empty")
	}
	if int64(len(data)) > policy.MaxBytes {
		return nil, "", errors.New("sticker media exceeds configured size limit")
	}
	mimeType, err := imageMIME(data)
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

func cloneTLSConfig(config *tls.Config) *tls.Config {
	if config == nil {
		return nil
	}
	return config.Clone()
}

func validateMediaURL(rawURL string, allowed []string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, policyError("sticker media must use an absolute HTTPS URL without credentials")
	}
	if !hostAllowed(parsed.Hostname(), allowed) {
		return nil, policyError("sticker media host is not allowlisted")
	}
	return parsed, nil
}

func normalizeAllowedHosts(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if strings.HasPrefix(value, "*.") {
			if strings.Count(value, "*") != 1 || len(value) <= 2 {
				return nil, fmt.Errorf("invalid sticker media host pattern %q", value)
			}
		} else if strings.Contains(value, "*") {
			return nil, fmt.Errorf("invalid sticker media host pattern %q", value)
		}
		if value == "" {
			continue
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("sticker media host allowlist is empty")
	}
	return result, nil
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, pattern := range allowed {
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

func resolvePublic(ctx context.Context, resolver IPResolver, host string) ([]net.IP, error) {
	if literal := net.ParseIP(host); literal != nil {
		if !isPublicIP(literal) {
			return nil, policyError("sticker media resolved to a non-public address")
		}
		return []net.IP{literal}, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, errors.New("resolve sticker media host failed")
	}
	if len(addresses) == 0 {
		return nil, errors.New("sticker media host has no address")
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			return nil, policyError("sticker media resolved to a non-public address")
		}
		result = append(result, append(net.IP(nil), address.IP...))
	}
	return result, nil
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func imageMIME(data []byte) (string, error) {
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", nil
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", nil
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif", nil
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", nil
	case len(data) >= 2 && bytes.Equal(data[:2], []byte("BM")):
		return "image/bmp", nil
	default:
		return "", errors.New("sticker media is not a supported JPEG, PNG, GIF, WebP, or BMP image")
	}
}
