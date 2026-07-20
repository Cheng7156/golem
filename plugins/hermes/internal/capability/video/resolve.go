package video

func (p *httpProvider) resolveURLCandidate(
	candidate ProviderCandidate,
	reference candidateReference,
) (ResolvedVideo, error) {
	mediaURL, err := p.validateMediaURL(reference.MediaURL)
	if err != nil {
		return ResolvedVideo{}, err
	}
	headers, err := p.mediaHeaders(reference.Request)
	if err != nil {
		return ResolvedVideo{}, err
	}
	return ResolvedVideo{
		Title: candidate.Title, PageURL: candidate.PageURL,
		FallbackURL:     p.FallbackURL(candidate),
		DurationSeconds: candidate.DurationSeconds,
		Source:          MediaSource{URL: mediaURL, Headers: headers},
	}, nil
}

func (p *httpProvider) firstResolved(decoded decodedResponse) (ResolvedVideo, error) {
	if decoded.binary != nil {
		return ResolvedVideo{Source: *decoded.binary}, nil
	}
	if len(decoded.candidates) == 0 {
		return ResolvedVideo{}, ErrNoCandidates
	}
	candidate := decoded.candidates[0]
	reference, err := parseCandidateReference(candidate.Reference)
	if err != nil {
		return ResolvedVideo{}, err
	}
	return p.resolveURLCandidate(candidate, reference)
}

func (p *httpProvider) FallbackURL(candidate ProviderCandidate) string {
	reference, _ := parseCandidateReference(candidate.Reference)
	switch p.config.FallbackURLPolicy {
	case "page_url":
		return candidate.PageURL
	case "media_url":
		return reference.MediaURL
	case "provider_public_url":
		return p.config.PublicFallbackURL
	default:
		return ""
	}
}

func (p *httpProvider) resolvedFallback(
	candidate ProviderCandidate,
	reference candidateReference,
	resolved ResolvedVideo,
) string {
	switch p.config.FallbackURLPolicy {
	case "page_url":
		if resolved.PageURL != "" {
			return resolved.PageURL
		}
		return candidate.PageURL
	case "media_url":
		if resolved.Source.URL != "" {
			return resolved.Source.URL
		}
		return reference.MediaURL
	case "provider_public_url":
		return p.config.PublicFallbackURL
	default:
		return ""
	}
}
