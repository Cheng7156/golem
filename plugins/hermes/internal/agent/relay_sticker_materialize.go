package agent

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

const maxStickerMaterializeBytes = 8 << 20

func (g *RelayGateway) serveStickerMaterialize(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerSelectRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker materialize request")
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
	if candidateID == "" {
		writeCapabilityError(w, http.StatusBadRequest, "candidate_id is empty")
		return
	}
	output, err := g.config.Stickers.Materialize(request.Context(), stickerScope(run), candidateID)
	if err != nil {
		slog.Warn("[hermes] sticker materialization failed", "run_id", run.request.RunID, "err", err)
		writeStickerMaterializeError(w, err)
		return
	}
	if len(output.Data) == 0 || len(output.Data) > maxStickerMaterializeBytes || !supportedStickerMIME(output.MIMEType) {
		writeCapabilityError(w, http.StatusBadGateway, "sticker provider returned invalid media")
		return
	}
	w.Header().Set("Content-Type", output.MIMEType)
	w.Header().Set("Content-Length", strconv.Itoa(len(output.Data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(output.Data); err != nil {
		slog.Warn("[hermes] write sticker materialization failed", "run_id", run.request.RunID, "err", err)
	}
}

func writeStickerMaterializeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrStickerCandidateUnavailable):
		writeCapabilityError(w, http.StatusNotFound, "sticker candidate is unavailable")
	case errors.Is(err, ErrStickerMaterializeCacheFull):
		writeCapabilityError(w, http.StatusServiceUnavailable, "sticker materialization cache is full")
	case errors.Is(err, ErrStickerProviderTimeout):
		writeCapabilityError(w, http.StatusGatewayTimeout, "sticker provider timed out")
	default:
		writeCapabilityError(w, http.StatusBadGateway, "sticker provider is unavailable")
	}
}

func supportedStickerMIME(value string) bool {
	switch strings.TrimSpace(value) {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return true
	default:
		return false
	}
}
