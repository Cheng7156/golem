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
	stickerLibraryInventoryPath = "/capabilities/v1/stickers/library/inventory"
	stickerLibrarySearchPath    = "/capabilities/v1/stickers/library/search"
	stickerLibraryPreviewPath   = "/capabilities/v1/stickers/library/preview"
	stickerLibraryPickPath      = "/capabilities/v1/stickers/library/pick"
	stickerLibraryCollectPath   = "/capabilities/v1/stickers/library/collect"
	stickerLibraryManagePath    = "/capabilities/v1/stickers/library/manage-recent"
	maxStickerDescriptionRunes  = 300
)

var (
	ErrStickerLibraryStorageFull  = errors.New("sticker library storage budget is exhausted")
	ErrStickerCollectionForbidden = errors.New("sticker collection is forbidden")
	ErrStickerManagementForbidden = errors.New("sticker library management is forbidden")
	ErrRecentStickerUnavailable   = errors.New("recent sent sticker is unavailable")
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

type StickerLibraryInventoryItem struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type StickerLibraryInventoryResult struct {
	Items            []StickerLibraryInventoryItem `json:"items"`
	Total            int                           `json:"total"`
	Limit            int                           `json:"limit"`
	Offset           int                           `json:"offset"`
	HasMore          bool                          `json:"has_more"`
	ExpiresInSeconds int                           `json:"expires_in"`
}

type StickerLibraryManageResult struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
}

type StickerLibraryCapability interface {
	AuthorizeCollection(context.Context, StickerScope) error
	InventoryLibrary(context.Context, StickerScope, int, int) (StickerLibraryInventoryResult, error)
	SearchLibrary(context.Context, StickerScope, string, int) (StickerSearchResult, error)
	Collect(context.Context, StickerScope, StickerLibraryCollection) (StickerLibraryCollectionResult, error)
	ManageRecentLibrarySticker(
		context.Context,
		StickerScope,
		string,
		string,
	) (StickerLibraryManageResult, error)
}

type stickerLibraryInventoryRequest struct {
	Limit   int                      `json:"limit"`
	Offset  int                      `json:"offset"`
	Context capabilitySessionContext `json:"context"`
}

type stickerLibraryPreviewRequest struct {
	Limit   int                      `json:"limit"`
	Offset  int                      `json:"offset"`
	Context capabilitySessionContext `json:"context"`
}

type stickerLibraryCollectRequest struct {
	CandidateID string                   `json:"candidate_id"`
	Description string                   `json:"description"`
	Context     capabilitySessionContext `json:"context"`
}

type stickerLibraryManageRequest struct {
	Action      string                   `json:"action"`
	Description string                   `json:"description,omitempty"`
	Context     capabilitySessionContext `json:"context"`
}

func (g *RelayGateway) serveStickerLibraryInventory(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerLibraryInventoryRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker library inventory request")
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
	if input.Limit <= 0 {
		input.Limit = 20
	}
	if input.Limit > 100 || input.Offset < 0 {
		writeCapabilityError(w, http.StatusBadRequest, "sticker library inventory range is invalid")
		return
	}
	result, err := g.config.StickerLibrary.InventoryLibrary(
		request.Context(), stickerScope(run), input.Limit, input.Offset,
	)
	if err != nil {
		slog.Warn("[hermes] sticker library inventory failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusServiceUnavailable, "sticker library inventory is temporarily unavailable")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
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

func (g *RelayGateway) serveStickerLibraryPreview(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerLibraryPreviewRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker library preview request")
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
	if input.Limit <= 0 {
		input.Limit = maxStickerSelections
	}
	if input.Limit > maxStickerSelections || input.Offset < 0 {
		writeCapabilityError(w, http.StatusBadRequest, "sticker library preview range is invalid")
		return
	}
	result, err := g.config.StickerLibrary.InventoryLibrary(
		request.Context(), stickerScope(run), input.Limit, input.Offset,
	)
	if err != nil {
		slog.Warn("[hermes] sticker library preview failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusServiceUnavailable, "sticker library preview is temporarily unavailable")
		return
	}
	if len(result.Items) > input.Limit {
		writeCapabilityError(w, http.StatusInternalServerError, "sticker library preview returned too many items")
		return
	}
	response := map[string]any{
		"staged": false, "staged_count": 0, "total": result.Total,
		"offset": input.Offset, "next_offset": input.Offset,
		"remaining_count": max(result.Total-input.Offset, 0),
		"has_more":        result.HasMore, "descriptions": []string{},
	}
	if len(result.Items) == 0 {
		writeCapabilityJSON(w, http.StatusOK, response)
		return
	}
	candidateIDs := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		candidateIDs = append(candidateIDs, item.ID)
	}
	proposals, descriptions, err := g.prepareStickerEffects(request.Context(), run, candidateIDs)
	if err != nil {
		slog.Warn("[hermes] sticker library preview selection failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusBadRequest, "sticker candidate is unavailable")
		return
	}
	if err := run.stageEffects(proposals); err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	nextOffset := input.Offset + len(proposals)
	response["staged"] = true
	response["staged_count"] = len(proposals)
	response["next_offset"] = nextOffset
	response["remaining_count"] = max(result.Total-nextOffset, 0)
	response["has_more"] = nextOffset < result.Total
	response["descriptions"] = descriptions
	response["effect_only_token"] = relayEffectOnlyToken
	writeCapabilityJSON(w, http.StatusOK, response)
}

func (g *RelayGateway) serveStickerLibraryPick(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerSearchRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker library pick request")
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
	if input.Limit <= 0 {
		input.Limit = 5
	}
	result, err := g.config.StickerLibrary.SearchLibrary(
		request.Context(), stickerScope(run), query, input.Limit,
	)
	if err != nil {
		slog.Warn("[hermes] sticker library pick failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusServiceUnavailable, "sticker library pick is temporarily unavailable")
		return
	}
	if len(result.Candidates) == 0 {
		writeCapabilityJSON(w, http.StatusOK, map[string]any{
			"staged": false, "match_count": 0,
		})
		return
	}
	proposals, descriptions, err := g.prepareStickerEffects(
		request.Context(), run, []string{result.Candidates[0].ID},
	)
	if err != nil {
		slog.Warn("[hermes] sticker library pick selection failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusBadRequest, "sticker candidate is unavailable")
		return
	}
	if err := run.stageEffects(proposals); err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"staged": true, "match_count": len(result.Candidates),
		"description": descriptions[0], "effect_only_token": relayEffectOnlyToken,
	})
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

func (g *RelayGateway) serveStickerLibraryManageRecent(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerLibraryManageRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker library management request")
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
	input.Action = strings.TrimSpace(input.Action)
	input.Description = strings.TrimSpace(input.Description)
	if input.Action != "update_description" && input.Action != "delete" {
		writeCapabilityError(w, http.StatusBadRequest, "sticker library management action is invalid")
		return
	}
	if input.Action == "update_description" && (input.Description == "" ||
		len([]rune(input.Description)) > maxStickerDescriptionRunes) {
		writeCapabilityError(w, http.StatusBadRequest, "description is empty or too long")
		return
	}
	if input.Action == "delete" && input.Description != "" {
		writeCapabilityError(w, http.StatusBadRequest, "delete does not accept a description")
		return
	}
	result, err := g.config.StickerLibrary.ManageRecentLibrarySticker(
		request.Context(), stickerScope(run), input.Action, input.Description,
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrStickerManagementForbidden):
			writeCapabilityError(w, http.StatusForbidden, "only the owner may manage stickers")
		case errors.Is(err, ErrRecentStickerUnavailable):
			writeCapabilityJSON(w, http.StatusOK, map[string]any{
				"found": false, "action": input.Action,
			})
		default:
			slog.Warn("[hermes] sticker library management failed", "run_id", run.request.RunID, "err", err)
			writeCapabilityError(w, http.StatusInternalServerError, "could not manage recent sticker")
		}
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"found": true, "action": result.Action, "description": result.Description,
	})
}
