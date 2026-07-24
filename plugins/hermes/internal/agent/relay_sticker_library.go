package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const (
	stickerLibrarySearchPath   = "/capabilities/v1/stickers/library/search"
	stickerLibraryCollectPath  = "/capabilities/v1/stickers/library/collect"
	maxStickerDescriptionRunes = 300
)

var (
	ErrStickerLibraryStorageFull  = errors.New("sticker library storage budget is exhausted")
	ErrStickerCollectionForbidden = errors.New("sticker collection is forbidden")
)

type StickerLibraryCollection struct {
	Description       string
	Data              []byte
	MIMEType          string
	SourceSessionID   string
	SourceEventID     string
	SourceMessageID   string
	SourceSpeakerID   string
	SourceSpeakerName string
	Collector         domain.Principal
}

type StickerLibraryCollectionResult struct {
	Description  string `json:"description"`
	AssetCreated bool   `json:"asset_created"`
	LabelCreated bool   `json:"label_created"`
}

type StickerLibraryCapability interface {
	AuthorizeCollection(context.Context, StickerScope) error
	SearchLibrary(context.Context, StickerScope, string, int) (StickerSearchResult, error)
	Collect(context.Context, StickerScope, StickerLibraryCollection) (StickerLibraryCollectionResult, error)
}

type stickerLibraryCollectRequest struct {
	CandidateID string                   `json:"candidate_id"`
	Description string                   `json:"description"`
	Context     capabilitySessionContext `json:"context"`
}

func (g *RelayGateway) serveStickerLibrarySearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerSearchRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker library search request")
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
	query := strings.TrimSpace(input.Query)
	if query == "" {
		writeCapabilityError(w, http.StatusBadRequest, "sticker library query is empty")
		return
	}
	result, err := g.config.StickerLibrary.SearchLibrary(
		request.Context(), stickerScope(run), query, input.Limit,
	)
	if err != nil {
		slog.Warn("[hermes] sticker library search failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusServiceUnavailable, "sticker library search is temporarily unavailable")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveStickerLibraryCollect(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerLibraryCollectRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker collection request")
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
	if err := g.config.StickerLibrary.AuthorizeCollection(request.Context(), stickerScope(run)); err != nil {
		if errors.Is(err, ErrStickerCollectionForbidden) {
			writeCapabilityError(w, http.StatusForbidden, "only the owner may collect stickers")
		} else {
			writeCapabilityError(w, http.StatusServiceUnavailable, "sticker collection is temporarily unavailable")
		}
		return
	}
	candidateID := strings.TrimSpace(input.CandidateID)
	description := strings.TrimSpace(input.Description)
	if candidateID == "" || len(candidateID) > 128 {
		writeCapabilityError(w, http.StatusBadRequest, "candidate_id is invalid")
		return
	}
	if description == "" || len([]rune(description)) > maxStickerDescriptionRunes {
		writeCapabilityError(w, http.StatusBadRequest, "description is empty or too long")
		return
	}
	candidate, data, mimeType, err := g.materializeRunImage(request.Context(), run, candidateID)
	if err != nil {
		if request.Context().Err() != nil {
			return
		}
		writeImageMaterializeError(w, err)
		return
	}
	result, err := g.config.StickerLibrary.Collect(
		request.Context(), stickerScope(run), StickerLibraryCollection{
			Description: description, Data: data, MIMEType: mimeType,
			SourceSessionID: run.request.SessionID, SourceEventID: candidate.EventID,
			SourceMessageID: candidate.PlatformMessageID, SourceSpeakerID: candidate.SpeakerID,
			SourceSpeakerName: candidate.SpeakerName, Collector: run.request.Principal,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrStickerCollectionForbidden):
			writeCapabilityError(w, http.StatusForbidden, "only the owner may collect stickers")
		case errors.Is(err, ErrStickerLibraryStorageFull):
			writeCapabilityError(w, http.StatusInsufficientStorage, "sticker library storage is full")
		default:
			slog.Warn("[hermes] sticker collection failed", "run_id", run.request.RunID, "err", err)
			writeCapabilityError(w, http.StatusInternalServerError, "could not collect sticker")
		}
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"stored":        true,
		"description":   result.Description,
		"asset_created": result.AssetCreated,
		"label_created": result.LabelCreated,
	})
}
