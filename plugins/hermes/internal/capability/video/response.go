package video

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

type decodedResponse struct {
	candidates []ProviderCandidate
	binary     *MediaSource
}

type decodeResponseRequest struct {
	response    *http.Response
	secrets     []string
	input       DiscoveryRequest
	allowBinary bool
}

func (p *httpProvider) decodeResponse(request decodeResponseRequest) (decodedResponse, error) {
	reader := bufio.NewReader(request.response.Body)
	format, err := p.responseFormat(request.response, reader)
	if err != nil {
		request.response.Body.Close()
		return decodedResponse{}, err
	}
	if format == "binary" {
		return p.decodeBinary(request.response, reader, request.allowBinary)
	}
	defer request.response.Body.Close()
	body, err := readBytesLimit(reader, p.config.MaxMetadataBytes)
	if err != nil {
		return decodedResponse{}, err
	}
	if format == "json" {
		return p.decodeJSON(body, request.secrets, request.input)
	}
	return p.decodeTextURL(body, request.secrets, request.input)
}

func (p *httpProvider) responseFormat(response *http.Response, reader *bufio.Reader) (string, error) {
	configured := p.config.ResponseMode
	peeked, _ := reader.Peek(512)
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if configured != "auto" {
		if configured == "binary" && looksLikeJSON(contentType, peeked) {
			return "json", nil
		}
		return configured, nil
	}
	if looksLikeJSON(contentType, peeked) {
		return "json", nil
	}
	if strings.HasPrefix(contentType, "video/") || looksLikeVideo(peeked) {
		return "binary", nil
	}
	if strings.HasPrefix(contentType, "text/") || looksLikeURL(peeked) {
		return "text_url", nil
	}
	if contentType == "application/octet-stream" {
		return "binary", nil
	}
	return "", fmt.Errorf("%w: content type %q", ErrUnsupportedFormat, contentType)
}

func (p *httpProvider) decodeBinary(
	response *http.Response,
	reader *bufio.Reader,
	allowBinary bool,
) (decodedResponse, error) {
	if !allowBinary {
		response.Body.Close()
		return decodedResponse{}, ErrUnexpectedBinary
	}
	if response.ContentLength > p.config.MaxSourceBytes {
		response.Body.Close()
		return decodedResponse{}, fmt.Errorf("video provider response exceeds %d bytes", p.config.MaxSourceBytes)
	}
	if !p.urlAllowed(response.Request.URL) {
		response.Body.Close()
		return decodedResponse{}, errors.New("video provider binary response URL is not allowed")
	}
	source := &MediaSource{
		Body:          readCloser{Reader: reader, Closer: response.Body},
		ContentType:   response.Header.Get("Content-Type"),
		ContentLength: response.ContentLength,
	}
	return decodedResponse{binary: source}, nil
}

func looksLikeJSON(contentType string, value []byte) bool {
	if contentType == "application/json" || strings.HasSuffix(contentType, "+json") {
		return true
	}
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func looksLikeURL(value []byte) bool {
	trimmed := strings.TrimSpace(string(value))
	return strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "http://")
}

func looksLikeVideo(value []byte) bool {
	if len(value) >= 12 && string(value[4:8]) == "ftyp" {
		return true
	}
	return len(value) >= 4 && string(value[:4]) == "\x1aE\xdf\xa3"
}

type readCloser struct {
	io.Reader
	io.Closer
}

func readBytesLimit(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	value, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read video provider response: %w", err)
	}
	if int64(len(value)) > maximum {
		return nil, fmt.Errorf("video provider metadata response exceeds %d bytes", maximum)
	}
	return value, nil
}

func readTextLimit(reader io.Reader, maximum int64) (string, error) {
	value, err := readBytesLimit(reader, maximum)
	return string(value), err
}
