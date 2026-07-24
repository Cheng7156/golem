package agent

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

const (
	imageSearchPath = "/capabilities/v1/images/search"
	imageReadPath   = "/capabilities/v1/images/read"

	defaultImageSearchLimit    = 8
	maxImageSearchLimit        = 16
	maxImageHistoryScan        = 64
	maxImageCandidates         = 64
	maxImageCacheBytes         = 32 << 20
	maxImageReadBytes          = 16 << 20
	maxImageQuestionBytes      = 2048
	maxConcurrentImageResolves = 4
	maxImageMaterializeTime    = 14 * time.Second
)

// InboundContextReader is intentionally narrower than the full Store.  The
// Relay can therefore expose a read-only view to a capability without giving
// the model access to persistence or run mutation operations.
type InboundContextReader interface {
	ListRecentInboundContext(context.Context, string, int64, int) ([]domain.ContextMessage, error)
}

// InboundMediaResolver materializes a verified WeChat media reference.  The
// implementation performs CDN/URL allowlisting, magic-byte checks and GIF
// normalization; this interface keeps those rules out of the model-facing
// HTTP handler.
type InboundMediaResolver interface {
	Resolve(context.Context, []domain.InboundMedia) ([]domain.InboundMedia, error)
}

type imageSearchRequest struct {
	SpeakerID   string                   `json:"speaker_id,omitempty"`
	SpeakerName string                   `json:"speaker_name,omitempty"`
	MessageID   string                   `json:"message_id,omitempty"`
	Limit       int                      `json:"limit,omitempty"`
	Context     capabilitySessionContext `json:"context"`
}

type imageReadRequest struct {
	CandidateID string                   `json:"candidate_id"`
	Question    string                   `json:"question,omitempty"`
	Context     capabilitySessionContext `json:"context"`
}

type relayImageCandidate struct {
	ID                string
	SourceKey         string
	EventID           string
	PlatformMessageID string
	AcceptSeq         int64
	OccurredAt        time.Time
	SpeakerID         string
	SpeakerName       string
	Kind              string
	MIMEType          string
	Media             domain.InboundMedia
	Readable          bool
	CreatedAt         time.Time
	CacheBytes        int64
	MaterializedData  []byte
	MaterializedMIME  string
}

type imageReadFlight struct {
	done     chan struct{}
	data     []byte
	mimeType string
	err      error
}

type imageCandidateResponse struct {
	ID               string `json:"id"`
	EventID          string `json:"event_id,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	AcceptSeq        int64  `json:"accept_seq,omitempty"`
	SpeakerID        string `json:"speaker_id,omitempty"`
	SpeakerName      string `json:"speaker_name,omitempty"`
	Kind             string `json:"kind"`
	MIMEType         string `json:"mime_type,omitempty"`
	OccurredAt       string `json:"occurred_at,omitempty"`
	Readable         bool   `json:"readable"`
	IsCurrentSender  bool   `json:"is_current_sender"`
	IsCurrentMessage bool   `json:"is_current_message"`
}

func (g *RelayGateway) serveImageSearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input imageSearchRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid image search request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	limit, err := normalizeImageSearchLimit(input.Limit)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	filters, err := normalizeImageFilters(input)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}
	candidates, err := g.searchRunImages(request.Context(), run, filters, limit)
	if err != nil {
		slog.Warn("[hermes] image metadata search failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusServiceUnavailable, "image context is temporarily unavailable")
		return
	}
	result := make([]imageCandidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		occurredAt := ""
		if !candidate.OccurredAt.IsZero() {
			occurredAt = candidate.OccurredAt.UTC().Format(time.RFC3339Nano)
		}
		result = append(result, imageCandidateResponse{
			ID:          candidate.ID,
			EventID:     candidate.EventID,
			MessageID:   candidate.PlatformMessageID,
			AcceptSeq:   candidate.AcceptSeq,
			SpeakerID:   candidate.SpeakerID,
			SpeakerName: candidate.SpeakerName,
			Kind:        candidate.Kind,
			MIMEType:    candidate.MIMEType,
			OccurredAt:  occurredAt,
			Readable:    candidate.Readable,
			IsCurrentSender: candidate.SpeakerID != "" &&
				candidate.SpeakerID == firstNonEmptyImage(
					run.request.CurrentMessage.SpeakerID,
					run.request.Principal.ID,
				),
			IsCurrentMessage: candidate.EventID != "" &&
				candidate.EventID == firstNonEmptyImage(
					run.request.CurrentEventID,
					run.request.MessageID,
				),
		})
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"candidates":         result,
		"expires_in":         120,
		"current_message_id": firstNonEmptyImage(run.request.CurrentEventID, run.request.MessageID),
		"current_sender": map[string]string{
			"speaker_id": firstNonEmptyImage(run.request.CurrentMessage.SpeakerID, run.request.Principal.ID),
			"speaker_name": firstNonEmptyImage(
				run.request.CurrentMessage.SpeakerName,
				run.request.Principal.Name,
			),
		},
	})
}

func (g *RelayGateway) serveImageRead(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input imageReadRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid image read request")
		return
	}
	if len([]byte(input.Question)) > maxImageQuestionBytes {
		writeCapabilityError(w, http.StatusBadRequest, "question is too long")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	if !authorizeInteractiveMedia(w, run) {
		return
	}
	candidateID := strings.TrimSpace(input.CandidateID)
	if candidateID == "" || len(candidateID) > 128 {
		writeCapabilityError(w, http.StatusBadRequest, "candidate_id is invalid")
		return
	}
	candidate, flight, owner, ok := run.beginImageRead(candidateID)
	if !ok {
		writeCapabilityError(w, http.StatusNotFound, "image candidate is unavailable")
		return
	}
	if !candidate.Readable {
		writeCapabilityError(w, http.StatusBadGateway, "image bytes are unavailable")
		return
	}
	if g.config.ImageResolver == nil {
		if owner {
			run.finishImageRead(candidateID, flight, nil, "", errors.New("image materialization is unavailable"))
		}
		writeCapabilityError(w, http.StatusServiceUnavailable, "image materialization is unavailable")
		return
	}
	if len(candidate.MaterializedData) > 0 {
		writeImagePayload(w, request, run, candidate.MaterializedData, candidate.MaterializedMIME)
		return
	}
	if !owner {
		select {
		case <-request.Context().Done():
			return
		case <-flight.done:
		}
		if flight.err != nil {
			writeCapabilityError(w, http.StatusBadGateway, "image materialization failed")
			return
		}
		writeImagePayload(w, request, run, flight.data, flight.mimeType)
		return
	}
	select {
	case g.imageResolveSlots <- struct{}{}:
		defer func() { <-g.imageResolveSlots }()
	case <-request.Context().Done():
		run.finishImageRead(candidateID, flight, nil, "", request.Context().Err())
		return
	default:
		run.finishImageRead(candidateID, flight, nil, "", errors.New("image materialization is busy"))
		w.Header().Set("Retry-After", "1")
		writeCapabilityError(w, http.StatusTooManyRequests, "image materialization is busy; retry shortly")
		return
	}
	resolveCtx, cancel := context.WithTimeout(request.Context(), maxImageMaterializeTime)
	defer cancel()
	resolved, err := g.config.ImageResolver.Resolve(resolveCtx, []domain.InboundMedia{cloneInboundMedia(candidate.Media)})
	if err != nil {
		run.finishImageRead(candidateID, flight, nil, "", err)
		slog.Warn("[hermes] image materialization failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusBadGateway, "image materialization failed")
		return
	}
	if len(resolved) != 1 {
		run.finishImageRead(candidateID, flight, nil, "", errors.New("image materialization returned an invalid result"))
		writeCapabilityError(w, http.StatusBadGateway, "image materialization returned an invalid result")
		return
	}
	data, mimeType, err := validateImagePayload(resolved[0])
	if err != nil {
		run.finishImageRead(candidateID, flight, nil, "", err)
		writeCapabilityError(w, http.StatusBadGateway, err.Error())
		return
	}
	run.finishImageRead(candidateID, flight, data, mimeType, nil)
	writeImagePayload(w, request, run, data, mimeType)
}

func writeImagePayload(
	w http.ResponseWriter,
	request *http.Request,
	run *relayRun,
	data []byte,
	mimeType string,
) {
	if err := request.Context().Err(); err != nil {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		slog.Debug("[hermes] image capability response write failed", "run_id", run.request.RunID, "err", err)
	}
}

type imageFilters struct {
	speakerID      string
	speakerName    string
	normalizedName string
	messageID      string
}

func normalizeImageFilters(input imageSearchRequest) (imageFilters, error) {
	filters := imageFilters{
		speakerID:   strings.TrimSpace(input.SpeakerID),
		speakerName: strings.TrimSpace(input.SpeakerName),
		messageID:   strings.TrimSpace(input.MessageID),
	}
	for label, value := range map[string]string{
		"speaker_id": filters.speakerID, "speaker_name": filters.speakerName,
		"message_id": filters.messageID,
	} {
		if len([]byte(value)) > 256 {
			return imageFilters{}, fmt.Errorf("%s is too long", label)
		}
	}
	filters.normalizedName = normalizeImageName(filters.speakerName)
	return filters, nil
}

func normalizeImageSearchLimit(value int) (int, error) {
	if value == 0 {
		return defaultImageSearchLimit, nil
	}
	if value < 1 || value > maxImageSearchLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxImageSearchLimit)
	}
	return value, nil
}

func (g *RelayGateway) searchRunImages(
	ctx context.Context,
	run *relayRun,
	filters imageFilters,
	limit int,
) ([]relayImageCandidate, error) {
	// Build a small newest-first view.  This performs no media resolution and
	// therefore has predictable latency even when the underlying CDN is slow.
	values := make([]imageContextValue, 0, limit)
	current := run.request.CurrentMessage
	if len(current.Media) == 0 && len(run.request.Media) > 0 {
		current.Media = append([]domain.InboundMedia(nil), run.request.Media...)
	}
	if len(current.Media) > 0 {
		values = append(values, imageContextValue{
			eventID:           firstNonEmptyImage(run.request.CurrentEventID, run.request.MessageID),
			platformMessageID: run.request.PlatformMessageID,
			acceptSeq:         run.request.CurrentAcceptSeq,
			occurredAt:        current.OccurredAt,
			speakerID:         firstNonEmptyImage(current.SpeakerID, run.request.Principal.ID),
			speakerName:       firstNonEmptyImage(current.SpeakerName, run.request.Principal.Name),
			media:             current.Media,
		})
	}
	if g.config.ImageContext != nil && run.request.CurrentAcceptSeq > 0 {
		history, err := g.config.ImageContext.ListRecentInboundContext(
			ctx, run.request.SessionID, run.request.CurrentAcceptSeq, maxImageHistoryScan,
		)
		if err != nil {
			return nil, err
		}
		for index := len(history) - 1; index >= 0; index-- {
			value := history[index]
			values = append(values, imageContextValue{
				eventID:           value.EventID,
				platformMessageID: value.PlatformMessageID,
				acceptSeq:         value.AcceptSeq,
				occurredAt:        value.OccurredAt,
				speakerID:         firstNonEmptyImage(value.Message.SpeakerID, value.Binding.Principal.ID),
				speakerName:       firstNonEmptyImage(value.Message.SpeakerName, value.Binding.Principal.Name),
				media:             value.Message.Media,
			})
		}
	}

	// Collect the bounded history before applying the presentation order.  The
	// history reader is newest-first, but that alone is not enough in a group:
	// a newer image from another participant can otherwise hide the image the
	// current sender is asking about.  Ranking below uses only connector-verified
	// event/speaker metadata; it never parses the user's text.
	result := make([]relayImageCandidate, 0, minImageInt(maxImageCandidates, limit*4))
	seen := make(map[string]struct{}, limit)
	for _, value := range values {
		if !imageSpeakerMatches(value, filters) || !imageMessageMatches(value, filters) {
			continue
		}
		for mediaIndex, media := range value.media {
			if !isInboundVisualKind(media.Kind) || !hasInboundMediaReference(media) {
				continue
			}
			sourceKey := imageSourceKey(value, mediaIndex)
			if _, exists := seen[sourceKey]; exists {
				continue
			}
			seen[sourceKey] = struct{}{}
			candidate, err := run.cacheImageCandidate(sourceKey, imageCandidateInput{
				eventID:           value.eventID,
				platformMessageID: value.platformMessageID,
				acceptSeq:         value.acceptSeq,
				occurredAt:        value.occurredAt,
				speakerID:         value.speakerID,
				speakerName:       value.speakerName,
				media:             media,
				mediaIndex:        mediaIndex,
			})
			if err != nil {
				return nil, err
			}
			result = append(result, candidate)
			if len(result) >= maxImageCandidates {
				break
			}
		}
		if len(result) >= maxImageCandidates {
			break
		}
	}
	orderImageCandidates(result, run, filters)
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func minImageInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// orderImageCandidates gives an unfiltered search a useful, deterministic
// default without making the tool infer intent from words such as "看图".
// The current message is strongest, then a candidate from the authenticated
// current sender, then the rest of the group.  Within a bucket, durable
// accept_seq/time and finally opaque ids provide a stable newest-first order.
func orderImageCandidates(
	values []relayImageCandidate,
	run *relayRun,
	filters imageFilters,
) {
	if len(values) < 2 || run == nil {
		return
	}
	preferSender := filters.speakerID == "" && filters.normalizedName == ""
	currentSender := firstNonEmptyImage(
		run.request.CurrentMessage.SpeakerID,
		run.request.Principal.ID,
	)
	currentEvent := firstNonEmptyImage(
		run.request.CurrentEventID,
		run.request.MessageID,
	)
	currentPlatformMessage := strings.TrimSpace(run.request.PlatformMessageID)
	rank := func(value relayImageCandidate) int {
		if currentEvent != "" && (value.EventID == currentEvent ||
			(value.PlatformMessageID != "" && value.PlatformMessageID == currentEvent)) {
			return 0
		}
		if currentPlatformMessage != "" && value.PlatformMessageID == currentPlatformMessage {
			return 0
		}
		if preferSender && currentSender != "" && value.SpeakerID == currentSender {
			return 1
		}
		return 2
	}
	sort.SliceStable(values, func(left, right int) bool {
		leftRank, rightRank := rank(values[left]), rank(values[right])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if values[left].AcceptSeq != values[right].AcceptSeq {
			return values[left].AcceptSeq > values[right].AcceptSeq
		}
		if !values[left].OccurredAt.Equal(values[right].OccurredAt) {
			return values[left].OccurredAt.After(values[right].OccurredAt)
		}
		if values[left].EventID != values[right].EventID {
			return values[left].EventID > values[right].EventID
		}
		if values[left].PlatformMessageID != values[right].PlatformMessageID {
			return values[left].PlatformMessageID > values[right].PlatformMessageID
		}
		return values[left].SourceKey > values[right].SourceKey
	})
}

type imageContextValue struct {
	eventID           string
	platformMessageID string
	acceptSeq         int64
	occurredAt        time.Time
	speakerID         string
	speakerName       string
	media             []domain.InboundMedia
}

type imageCandidateInput struct {
	eventID           string
	platformMessageID string
	acceptSeq         int64
	occurredAt        time.Time
	speakerID         string
	speakerName       string
	media             domain.InboundMedia
	mediaIndex        int
}

func imageSpeakerMatches(value imageContextValue, filters imageFilters) bool {
	if filters.speakerID != "" && value.speakerID != filters.speakerID {
		return false
	}
	return filters.normalizedName == "" || normalizeImageName(value.speakerName) == filters.normalizedName
}

func imageMessageMatches(value imageContextValue, filters imageFilters) bool {
	if filters.messageID == "" {
		return true
	}
	return filters.messageID == strings.TrimSpace(value.eventID) ||
		filters.messageID == strings.TrimSpace(value.platformMessageID)
}

func imageSourceKey(value imageContextValue, mediaIndex int) string {
	identity := firstNonEmptyImage(value.eventID, value.platformMessageID)
	if identity == "" {
		identity = "seq:" + strconv.FormatInt(value.acceptSeq, 10)
	}
	// Include the verified speaker as well as the event identity.  This keeps
	// two senders distinct even on hosts that reuse a platform message id or
	// omit the durable event id.
	return identity + ":" + strings.TrimSpace(value.speakerID) + ":" + strconv.Itoa(mediaIndex)
}

func (r *relayRun) cacheImageCandidate(sourceKey string, input imageCandidateInput) (relayImageCandidate, error) {
	r.imageMu.Lock()
	defer r.imageMu.Unlock()
	if r.imageCandidates == nil {
		r.imageCandidates = make(map[string]relayImageCandidate)
	}
	if r.imageBySource == nil {
		r.imageBySource = make(map[string]string)
	}
	if existingID := r.imageBySource[sourceKey]; existingID != "" {
		if existing, ok := r.imageCandidates[existingID]; ok {
			return existing, nil
		}
		delete(r.imageBySource, sourceKey)
	}
	media := cloneInboundMedia(input.media)
	cacheBytes := int64(len(media.Data) + len(media.DownloadSource))
	readable := hasInboundMediaReference(media)
	if cacheBytes > maxImageCacheBytes {
		media.Data = nil
		media.DownloadSource = nil
		cacheBytes = 0
		readable = strings.TrimSpace(media.URL) != ""
	}
	for r.imageCacheBytes+cacheBytes > maxImageCacheBytes && len(r.imageOrder) > 0 {
		r.evictOldestImageCandidateLocked()
	}
	id := ""
	for attempts := 0; attempts < 4; attempts++ {
		candidateID, err := newImageCandidateID()
		if err != nil {
			return relayImageCandidate{}, err
		}
		if _, exists := r.imageCandidates[candidateID]; !exists {
			id = candidateID
			break
		}
	}
	if id == "" {
		return relayImageCandidate{}, errors.New("could not allocate a unique image candidate id")
	}
	now := time.Now().UTC()
	candidate := relayImageCandidate{
		ID:                id,
		SourceKey:         sourceKey,
		EventID:           input.eventID,
		PlatformMessageID: input.platformMessageID,
		AcceptSeq:         input.acceptSeq,
		OccurredAt:        input.occurredAt,
		SpeakerID:         input.speakerID,
		SpeakerName:       input.speakerName,
		Kind:              strings.ToLower(strings.TrimSpace(media.Kind)),
		MIMEType:          strings.TrimSpace(media.MIMEType),
		Media:             media,
		Readable:          readable,
		CreatedAt:         now,
		CacheBytes:        cacheBytes,
	}
	r.imageCandidates[id] = candidate
	r.imageBySource[sourceKey] = id
	r.imageOrder = append(r.imageOrder, id)
	r.imageCacheBytes += cacheBytes
	for len(r.imageOrder) > maxImageCandidates {
		r.evictOldestImageCandidateLocked()
	}
	return candidate, nil
}

func (r *relayRun) evictOldestImageCandidateLocked() {
	if len(r.imageOrder) == 0 {
		return
	}
	id := r.imageOrder[0]
	r.imageOrder = r.imageOrder[1:]
	candidate, ok := r.imageCandidates[id]
	if !ok {
		return
	}
	delete(r.imageCandidates, id)
	if r.imageBySource[candidate.SourceKey] == id {
		delete(r.imageBySource, candidate.SourceKey)
	}
	r.imageCacheBytes -= candidate.CacheBytes
	if r.imageCacheBytes < 0 {
		r.imageCacheBytes = 0
	}
}

func (r *relayRun) imageCandidate(id string) (relayImageCandidate, bool) {
	r.imageMu.Lock()
	defer r.imageMu.Unlock()
	candidate, ok := r.imageCandidates[id]
	if !ok {
		return relayImageCandidate{}, false
	}
	candidate.Media = cloneInboundMedia(candidate.Media)
	candidate.MaterializedData = append([]byte(nil), candidate.MaterializedData...)
	return candidate, true
}

func (r *relayRun) beginImageRead(
	id string,
) (relayImageCandidate, *imageReadFlight, bool, bool) {
	r.imageMu.Lock()
	defer r.imageMu.Unlock()
	candidate, ok := r.imageCandidates[id]
	if !ok {
		return relayImageCandidate{}, nil, false, false
	}
	candidate.Media = cloneInboundMedia(candidate.Media)
	candidate.MaterializedData = append([]byte(nil), candidate.MaterializedData...)
	if !candidate.Readable || len(candidate.MaterializedData) > 0 {
		return candidate, nil, false, true
	}
	if r.imageFlights == nil {
		r.imageFlights = make(map[string]*imageReadFlight)
	}
	if flight := r.imageFlights[id]; flight != nil {
		return candidate, flight, false, true
	}
	flight := &imageReadFlight{done: make(chan struct{})}
	r.imageFlights[id] = flight
	return candidate, flight, true, true
}

func (r *relayRun) finishImageRead(
	id string,
	flight *imageReadFlight,
	data []byte,
	mimeType string,
	err error,
) {
	if flight == nil {
		return
	}
	r.imageMu.Lock()
	current := r.imageFlights[id]
	if current != flight {
		r.imageMu.Unlock()
		return
	}
	delete(r.imageFlights, id)
	flight.err = err
	if err == nil {
		flight.data = append([]byte(nil), data...)
		flight.mimeType = mimeType
		if candidate, ok := r.imageCandidates[id]; ok {
			oldBytes := candidate.CacheBytes
			candidate.Media.Data = nil
			candidate.Media.DownloadSource = nil
			candidate.Media.MIMEType = mimeType
			candidate.MIMEType = mimeType
			candidate.MaterializedData = append([]byte(nil), data...)
			candidate.MaterializedMIME = mimeType
			candidate.CacheBytes = int64(len(candidate.MaterializedData))
			r.imageCandidates[id] = candidate
			r.imageCacheBytes += candidate.CacheBytes - oldBytes
			for r.imageCacheBytes > maxImageCacheBytes && len(r.imageOrder) > 1 {
				if r.imageOrder[0] == id {
					r.imageOrder = append(r.imageOrder[1:], id)
					continue
				}
				r.evictOldestImageCandidateLocked()
			}
		}
	}
	close(flight.done)
	r.imageMu.Unlock()
}

func newImageCandidateID() (string, error) {
	var raw [18]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", errors.New("could not allocate image candidate id")
	}
	return "img_" + hex.EncodeToString(raw[:]), nil
}

func cloneInboundMedia(media domain.InboundMedia) domain.InboundMedia {
	clone := media
	clone.Data = append([]byte(nil), media.Data...)
	clone.DownloadSource = append([]byte(nil), media.DownloadSource...)
	return clone
}

func isInboundVisualKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return kind == "image" || kind == "emoji"
}

func hasInboundMediaReference(media domain.InboundMedia) bool {
	return len(media.Data) > 0 || len(media.DownloadSource) > 0 || strings.TrimSpace(media.URL) != ""
}

func normalizeImageName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), ""))
}

func firstNonEmptyImage(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func validateImagePayload(media domain.InboundMedia) ([]byte, string, error) {
	if len(media.Data) == 0 {
		return nil, "", errors.New("image materialization returned empty media")
	}
	if len(media.Data) > maxImageReadBytes {
		return nil, "", errors.New("image materialization exceeds 16 MiB")
	}
	mimeType := strings.ToLower(strings.TrimSpace(media.MIMEType))
	if !supportedInboundImageMIME(mimeType) {
		return nil, "", errors.New("image materialization returned unsupported media")
	}
	detected := http.DetectContentType(media.Data)
	if detected == "image/svg+xml" || !strings.HasPrefix(detected, "image/") {
		return nil, "", errors.New("image materialization failed magic-byte validation")
	}
	// DetectContentType may include a charset parameter for unusual formats;
	// the resolver's MIME is authoritative only when it remains in the same
	// supported raster family.
	if detected != mimeType && !(mimeType == "image/jpeg" && detected == "image/jpg") {
		return nil, "", errors.New("image materialization MIME does not match its bytes")
	}
	return media.Data, mimeType, nil
}

func supportedInboundImageMIME(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return true
	default:
		return false
	}
}
