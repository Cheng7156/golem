package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

const (
	relayStickerIntentExample = "[[GOLEM_HERMES_STICKER_INTENT_V1:无语]]"
	relayStickerIntentLimit   = 5
	relayStickerIntentTimeout = 5 * time.Second
)

var (
	relayStickerIntentPattern = regexp.MustCompile(
		`(?i)\[\[\s*GOLEM_HERMES_STICKER_INTENT_V1\s*(?::\s*([^\]\r\n]*))?\s*\]\]`,
	)
	relayStickerIntentKeywords = map[string]struct{}{
		"无语": {}, "嘲笑": {}, "闭嘴": {}, "疑惑": {},
		"震惊": {}, "嫌弃": {}, "得意": {}, "生气": {},
		"尴尬": {}, "赞同": {}, "拒绝": {}, "看戏": {},
	}
)

func parseRelayStickerIntent(content string) (string, string) {
	matches := relayStickerIntentPattern.FindAllStringSubmatch(content, -1)
	keyword := ""
	for _, match := range matches {
		if keyword != "" || len(match) < 2 {
			continue
		}
		candidate := strings.TrimSpace(match[1])
		if _, allowed := relayStickerIntentKeywords[candidate]; allowed {
			keyword = candidate
		}
	}
	return strings.TrimSpace(relayStickerIntentPattern.ReplaceAllString(content, "")), keyword
}

func (g *RelayGateway) resolveStickerIntent(
	ctx context.Context,
	run *relayRun,
	keyword string,
) (*OutputProposal, error) {
	if keyword == "" || run == nil || g.config.Stickers == nil ||
		run.request.TriggerKind == domain.TriggerAmbient {
		return nil, nil
	}
	intentCtx, cancel := context.WithTimeout(ctx, relayStickerIntentTimeout)
	defer cancel()
	result, err := g.config.Stickers.Search(
		intentCtx, stickerScope(run), keyword, relayStickerIntentLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("search sticker intent %q: %w", keyword, err)
	}
	if len(result.Candidates) == 0 {
		return nil, fmt.Errorf("search sticker intent %q: no candidates", keyword)
	}
	index, err := randomStickerCandidateIndex(len(result.Candidates))
	if err != nil {
		return nil, err
	}
	candidate := result.Candidates[index]
	output, err := g.config.Stickers.Select(intentCtx, stickerScope(run), candidate.ID)
	if err != nil {
		return nil, fmt.Errorf("select sticker intent %q: %w", keyword, err)
	}
	if len(output.Data) == 0 || len(output.Data) > maxStickerMaterializeBytes ||
		!supportedStickerMIME(output.MIMEType) {
		return nil, errors.New("selected sticker intent returned invalid media")
	}
	payload, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode sticker intent effect: %w", err)
	}
	proposal := &OutputProposal{Kind: "emoji", Payload: payload}
	slog.Info("[hermes] 自动表情意图已选图",
		"run_id", run.request.RunID,
		"session_id", run.request.SessionID,
		"keyword", keyword,
		"description", output.Description,
	)
	return proposal, nil
}

func randomStickerCandidateIndex(count int) (int, error) {
	if count <= 0 {
		return 0, errors.New("sticker candidate count must be positive")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return 0, fmt.Errorf("choose random sticker candidate: %w", err)
	}
	return int(value.Int64()), nil
}

func containsEffectKind(proposals []OutputProposal, kind string) bool {
	for _, proposal := range proposals {
		if proposal.Kind == kind {
			return true
		}
	}
	return false
}

func (r *relayRun) hasEffectKind(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return containsEffectKind(r.effects, kind)
}
