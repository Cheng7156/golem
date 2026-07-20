package video

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

const directURLProviderID = "direct_url"

type directURLProvider struct {
	allowHTTP bool
}

func (p directURLProvider) ID() string {
	return directURLProviderID
}

func (p directURLProvider) Categories() []string {
	return []string{"*"}
}

func (p directURLProvider) Discover(context.Context, DiscoveryRequest) ([]ProviderCandidate, error) {
	return nil, errors.New("direct URL provider does not support discovery")
}

func (p directURLProvider) Resolve(
	_ context.Context,
	candidate ProviderCandidate,
) (ResolvedVideo, error) {
	reference, err := parseCandidateReference(candidate.Reference)
	if err != nil {
		return ResolvedVideo{}, err
	}
	mediaURL, err := validateDirectURL(reference.MediaURL, p.allowHTTP)
	if err != nil {
		return ResolvedVideo{}, err
	}
	return ResolvedVideo{
		Title: candidate.Title, PageURL: candidate.PageURL, FallbackURL: mediaURL,
		DurationSeconds: candidate.DurationSeconds, Source: MediaSource{URL: mediaURL},
	}, nil
}

func (p directURLProvider) FallbackURL(candidate ProviderCandidate) string {
	reference, err := parseCandidateReference(candidate.Reference)
	if err != nil {
		return ""
	}
	value, _ := validateDirectURL(reference.MediaURL, p.allowHTTP)
	return value
}

func newDirectURLCandidate(rawURL, title string, allowHTTP bool) (ProviderCandidate, error) {
	mediaURL, err := validateDirectURL(rawURL, allowHTTP)
	if err != nil {
		return ProviderCandidate{}, err
	}
	reference, err := json.Marshal(candidateReference{MediaURL: mediaURL})
	if err != nil {
		return ProviderCandidate{}, err
	}
	return ProviderCandidate{
		Reference: string(reference), Title: strings.TrimSpace(title), PageURL: mediaURL,
	}, nil
}

func validateDirectURL(value string, allowHTTP bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("video URL must be absolute and contain no credentials")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return "", errors.New("video URL must use HTTPS")
	}
	return parsed.String(), nil
}
