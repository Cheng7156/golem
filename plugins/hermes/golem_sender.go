package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/mediaobject"
	"golem_plugin_hermes/internal/output"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

const maxOutboundMediaBytes = 8 << 20
const maxOutboundVideoEnvelopeBytes = 48 << 20

type golemSender struct {
	ability       message.Ability
	fallbackSlots chan struct{}
	mediaObjects  mediaobject.Reader
}

func newGolemSender(
	ability message.Ability,
	fallbackConcurrency int,
	mediaObjects mediaobject.Reader,
) *golemSender {
	if fallbackConcurrency <= 0 {
		fallbackConcurrency = 4
	}
	return &golemSender{
		ability:       ability,
		fallbackSlots: make(chan struct{}, fallbackConcurrency),
		mediaObjects:  mediaObjects,
	}
}

func (s *golemSender) Send(ctx context.Context, item domain.OutboxItem) (output.Receipt, error) {
	if s.ability == nil {
		return output.Receipt{}, errors.New("message ability is not registered")
	}
	msg, err := s.outboxMessage(ctx, item)
	if err != nil {
		return output.Receipt{}, output.PermanentError{Err: err}
	}
	if client, ok := s.ability.(message.Client); ok {
		stream, err := client.Client.Send(ctx)
		if err != nil {
			return output.Receipt{}, err
		}
		if err := stream.Send(&message.Send_Request{Message: msg}); err != nil {
			return output.Receipt{}, classifySendError(err, true)
		}
		response, err := stream.CloseAndRecv()
		if err != nil {
			return output.Receipt{}, classifySendError(err, true)
		}
		return receipt(response)
	}

	select {
	case s.fallbackSlots <- struct{}{}:
	case <-ctx.Done():
		return output.Receipt{}, ctx.Err()
	}
	type result struct {
		response *message.Send_Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-s.fallbackSlots }()
		response, err := s.ability.Send(msg)
		done <- result{response: response, err: err}
	}()
	select {
	case <-ctx.Done():
		return output.Receipt{}, output.AmbiguousError{Err: ctx.Err()}
	case value := <-done:
		if value.err != nil {
			return output.Receipt{}, classifySendError(value.err, true)
		}
		return receipt(value.response)
	}
}

func (s *golemSender) outboxMessage(ctx context.Context, item domain.OutboxItem) (*message.Message, error) {
	receiver := &contact.Contact{Username: item.ReceiverID}
	// Image and emoji bytes intentionally stay inside message.Send. The Host
	// owns its CDN upload; this plugin must never pre-upload via cdn.Ability or
	// replace bytes with file_id/key metadata.
	switch item.Kind {
	case "text":
		return textOutboxMessage(receiver, item.Payload)
	case "image":
		return imageOutboxMessage(ctx, receiver, item.Payload)
	case "emoji":
		return emojiOutboxMessage(ctx, receiver, item.Payload)
	case "video":
		return s.videoMessage(ctx, receiver, item.Payload)
	default:
		return nil, fmt.Errorf("unsupported outbox kind=%s", item.Kind)
	}
}

func receipt(response *message.Send_Response) (output.Receipt, error) {
	if response == nil {
		return output.Receipt{}, output.AmbiguousError{
			Err: errors.New("message sender returned a nil response"),
		}
	}
	if response.GetNewId() == 0 {
		return output.Receipt{}, output.AmbiguousError{
			Err: errors.New("message sender returned a zero receipt id"),
		}
	}
	return output.Receipt{
		ID:        response.GetNewId(),
		CreatedAt: messageTime(response.GetCreateTime()),
	}, nil
}

func loadOutboundMedia(ctx context.Context, rawURL string, inline []byte) ([]byte, error) {
	if len(inline) > 0 {
		if len(inline) > maxOutboundMediaBytes {
			return nil, errors.New("inline media exceeds 8 MiB")
		}
		return append([]byte(nil), inline...), nil
	}
	return downloadOutboundMedia(ctx, rawURL)
}

func downloadOutboundMedia(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, errors.New("remote media must use an absolute HTTPS URL without credentials")
	}
	client, transport := newOutboundMediaClient()
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "golem-hermes/1.0")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("media server returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxOutboundMediaBytes {
		return nil, errors.New("remote media exceeds 8 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxOutboundMediaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxOutboundMediaBytes {
		return nil, errors.New("remote media is empty or exceeds 8 MiB")
	}
	return data, nil
}

func newOutboundMediaClient() (*http.Client, *http.Transport) {
	transport := &http.Transport{
		ForceAttemptHTTP2: true, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
		DialContext: dialPublicAddress,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many media redirects")
			}
			if request.URL.Scheme != "https" || request.URL.User != nil {
				return errors.New("media redirect must remain on HTTPS without credentials")
			}
			return nil
		},
	}
	return client, transport
}

func dialPublicAddress(ctx context.Context, network, address string) (net.Conn, error) {
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
		if !publicIP(candidate.IP) {
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
	return nil, errors.New("media host did not resolve to a public address")
}

func publicIP(ip net.IP) bool {
	return ip != nil &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast()
}

func validateImage(data []byte) error {
	detected := strings.ToLower(http.DetectContentType(data))
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/bmp":
		return nil
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return nil
	}
	return fmt.Errorf("unsupported media image type %s", detected)
}
