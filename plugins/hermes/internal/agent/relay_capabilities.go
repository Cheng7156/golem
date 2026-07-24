package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const (
	stickerSearchPath      = "/capabilities/v1/stickers/search"
	stickerMaterializePath = "/capabilities/v1/stickers/materialize"
	stickerSelectPath      = "/capabilities/v1/stickers/select"
	maxCapabilityBody      = 32 << 10
	ambientMediaDenied     = "media capability is unavailable for unaddressed ambient runs"
)

var (
	ErrStickerCandidateUnavailable = errors.New("sticker candidate is unavailable")
	ErrStickerMaterializeCacheFull = errors.New("sticker materialized cache is full")
	ErrStickerProviderTimeout      = errors.New("sticker provider timed out")
	ErrStickerProviderUnavailable  = errors.New("sticker provider is unavailable")
)

type StickerScope struct {
	RunID     string
	SessionID string
	ChatID    string
	Principal domain.Principal
}

type StickerCandidate struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Format      string `json:"format,omitempty"`
	Animated    bool   `json:"animated,omitempty"`
}

type StickerSearchResult struct {
	Candidates       []StickerCandidate `json:"candidates"`
	ExpiresInSeconds int                `json:"expires_in"`
}

type StickerCapability interface {
	Search(context.Context, StickerScope, string, int) (StickerSearchResult, error)
	Materialize(context.Context, StickerScope, string) (domain.EmojiOutput, error)
	Select(context.Context, StickerScope, string) (domain.EmojiOutput, error)
}

type capabilitySessionContext struct {
	Platform   string `json:"platform"`
	ChatID     string `json:"chat_id"`
	ThreadID   string `json:"thread_id,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	MessageID  string `json:"message_id,omitempty"`
	Profile    string `json:"profile,omitempty"`
}

type stickerSearchRequest struct {
	Query   string                   `json:"query"`
	Limit   int                      `json:"limit"`
	Context capabilitySessionContext `json:"context"`
}

type stickerSelectRequest struct {
	CandidateID string                   `json:"candidate_id"`
	Context     capabilitySessionContext `json:"context"`
}

func (g *RelayGateway) serveStickerSearch(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerSearchRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker search request")
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
		writeCapabilityError(w, http.StatusBadRequest, "sticker query is empty")
		return
	}
	result, err := g.config.Stickers.Search(request.Context(), stickerScope(run), query, input.Limit)
	if err != nil {
		slog.Warn("[hermes] sticker search failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusBadGateway, "sticker search is temporarily unavailable")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, result)
}

func (g *RelayGateway) serveStickerSelect(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input stickerSelectRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid sticker selection request")
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
	output, err := g.config.Stickers.Select(request.Context(), stickerScope(run), candidateID)
	if err != nil {
		slog.Warn("[hermes] sticker selection failed", "run_id", run.request.RunID, "err", err)
		writeCapabilityError(w, http.StatusBadRequest, "sticker candidate is unavailable")
		return
	}
	payload, err := json.Marshal(output)
	if err != nil {
		writeCapabilityError(w, http.StatusInternalServerError, "could not stage sticker")
		return
	}
	if err := run.stageEffect(OutputProposal{Kind: "emoji", Payload: payload}); err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"staged":            true,
		"effect_only_token": relayEffectOnlyToken,
		"description":       output.Description,
	})
}

func (g *RelayGateway) prepareCapabilityRequest(w http.ResponseWriter, request *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		writeCapabilityError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	const prefix = "Bearer "
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) || !constantTimeEqual(strings.TrimSpace(strings.TrimPrefix(header, prefix)), g.config.CapabilityToken) {
		writeCapabilityError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (g *RelayGateway) capabilityRun(value capabilitySessionContext) (*relayRun, error) {
	if !strings.EqualFold(strings.TrimSpace(value.Platform), "relay") {
		return nil, errors.New("capability is only available for relay sessions")
	}
	chatID := strings.TrimSpace(value.ChatID)
	sessionKey := strings.TrimSpace(value.SessionKey)
	userID := strings.TrimSpace(value.UserID)
	messageID := strings.TrimSpace(value.MessageID)
	if chatID == "" || sessionKey == "" || userID == "" || messageID == "" {
		return nil, errors.New("chat context is missing")
	}
	g.mu.Lock()
	runs := g.pendingRunsForChatLocked(chatID)
	g.mu.Unlock()
	if len(runs) == 0 {
		return nil, errors.New("no active run for this chat")
	}
	var matched *relayRun
	for _, candidate := range runs {
		identity := relaySessionIdentity{
			request: candidate.request, chatID: candidate.chatID, profile: value.Profile,
		}
		if !identity.matches(sessionKey) || userID != candidate.request.Principal.ID ||
			(messageID != candidate.request.MessageID && messageID != candidate.request.PlatformMessageID) {
			continue
		}
		if matched != nil {
			return nil, ErrGatewayRunAmbiguous
		}
		matched = candidate
	}
	if matched == nil {
		return nil, errors.New("session context does not match the active run")
	}
	return matched, nil
}

// authorizeInteractiveMedia keeps the model-visible toolset stable while
// enforcing side-effect authority from the connector-authenticated Run.  The
// request body is deliberately not consulted: chat IDs, roles, and trigger
// claims supplied by a tool call are not an authorization source.
func authorizeInteractiveMedia(w http.ResponseWriter, run *relayRun) bool {
	if run != nil && run.request.TriggerKind == domain.TriggerAmbient {
		writeCapabilityError(w, http.StatusForbidden, ambientMediaDenied)
		return false
	}
	return true
}

func stickerScope(run *relayRun) StickerScope {
	return StickerScope{
		RunID:     run.request.RunID,
		SessionID: run.request.SessionID,
		ChatID:    run.chatID,
		Principal: run.request.Principal,
	}
}

func decodeCapabilityRequest(w http.ResponseWriter, request *http.Request, target any) error {
	return decodeCapabilityRequestLimit(w, request, target, maxCapabilityBody)
}

func decodeCapabilityRequestLimit(
	w http.ResponseWriter,
	request *http.Request,
	target any,
	limit int64,
) error {
	request.Body = http.MaxBytesReader(w, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("capability request must contain exactly one JSON value")
	}
	return nil
}

func writeCapabilityJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeCapabilityError(w http.ResponseWriter, status int, message string) {
	writeCapabilityJSON(w, status, map[string]any{"error": message})
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) || left == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
