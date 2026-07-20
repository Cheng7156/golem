package video

import (
	"encoding/json"
	"strings"
)

func (p *httpProvider) decodeTextURL(
	body []byte,
	secrets []string,
	input DiscoveryRequest,
) (decodedResponse, error) {
	value := strings.TrimSpace(string(body))
	value, err := applyTransforms(value, p.config.Response.URL.Transforms)
	if err != nil {
		return decodedResponse{}, err
	}
	mediaURL, err := p.validateMediaURL(value)
	if err != nil {
		return decodedResponse{}, err
	}
	reference, err := json.Marshal(candidateReference{MediaURL: mediaURL, Request: input})
	if err != nil {
		return decodedResponse{}, err
	}
	title := redact(strings.TrimSpace(p.config.ProviderID+" 视频"), secrets)
	return decodedResponse{candidates: []ProviderCandidate{{
		Reference: string(reference), Title: title,
	}}}, nil
}
