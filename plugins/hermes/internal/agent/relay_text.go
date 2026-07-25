package agent

import (
	"encoding/json"
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode"

	"golem_plugin_hermes/internal/domain"
)

func newRelayTextProposal(content string) (string, *OutputProposal, error) {
	return newRelayTextProposalWithDelivery(content, domain.DeliveryTarget{})
}

func newRelayTextProposalWithDelivery(
	content string,
	delivery domain.DeliveryTarget,
) (string, *OutputProposal, error) {
	visibleContent := markdownToWeChatText(stripRelayInternalTokens(content))
	if visibleContent == "" {
		return "", nil, errors.New("reply is empty after Markdown normalization")
	}
	proposal, err := NewTextProposal(visibleContent)
	if err != nil {
		return "", nil, err
	}
	if !delivery.Empty() {
		var output domain.TextOutput
		if err := json.Unmarshal(proposal.Payload, &output); err != nil {
			return "", nil, err
		}
		output.Delivery = &delivery
		proposal.Payload, err = json.Marshal(output)
		if err != nil {
			return "", nil, err
		}
	}
	return visibleContent, &proposal, nil
}

func normalizeDeliveryTarget(
	request RunRequest,
	supplied *domain.DeliveryTarget,
) (domain.DeliveryTarget, error) {
	var target domain.DeliveryTarget
	if supplied != nil {
		target = *supplied
	}
	target.ReplyToMessageID = strings.TrimSpace(target.ReplyToMessageID)
	target.MentionActorID = strings.TrimSpace(target.MentionActorID)
	target.MentionActorName = strings.TrimSpace(target.MentionActorName)
	expectedMessageID := strings.TrimSpace(request.PlatformMessageID)
	if target.ReplyToMessageID != "" && target.ReplyToMessageID != expectedMessageID {
		return domain.DeliveryTarget{}, errors.New("delivery reply target does not match the triggering message")
	}
	if target.MentionActorID != "" && target.MentionActorID != request.VerifiedActor.ActorID {
		return domain.DeliveryTarget{}, errors.New("delivery mention target does not match the verified actor")
	}
	if target.MentionActorName != "" && target.MentionActorName != request.VerifiedActor.DisplayName {
		return domain.DeliveryTarget{}, errors.New("delivery mention name does not match the verified actor")
	}
	target.ReplyToMessageID = expectedMessageID
	if request.ChatType == "group" && request.RequireVisibleReply {
		target.MentionActorID = strings.TrimSpace(request.VerifiedActor.ActorID)
		target.MentionActorName = strings.TrimSpace(request.VerifiedActor.DisplayName)
	} else {
		target.MentionActorID = ""
		target.MentionActorName = ""
	}
	return target, nil
}

var (
	markdownHeadingPattern   = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	markdownQuotePattern     = regexp.MustCompile(`^\s{0,3}(?:>\s*)+`)
	markdownBulletPattern    = regexp.MustCompile(`^\s*[-+*]\s+`)
	markdownRulePattern      = regexp.MustCompile(`^\s{0,3}(?:[-*_]\s*){3,}$`)
	markdownTableRulePattern = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$`)
	markdownImagePattern     = regexp.MustCompile(`!\[([^]\n]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	markdownLinkPattern      = regexp.MustCompile(`\[([^]\n]+)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	markdownInlineCode       = regexp.MustCompile("`([^`\\n]+)`")
	markdownBoldAsterisk     = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	markdownBoldUnderscore   = regexp.MustCompile(`__([^_\n]+)__`)
	markdownStrikePattern    = regexp.MustCompile(`~~([^~\n]+)~~`)
	markdownItalicAsterisk   = regexp.MustCompile(`\*([^*\s](?:[^*\n]*[^*\s])?)\*`)
	markdownEscapePattern    = regexp.MustCompile(`\\([\\` + "`" + `*_[\]{}()#+.!|>~-])`)
)

var (
	relayInternalTokenPattern = regexp.MustCompile(
		"(?i)[`*_~\"'“”‘’]*\\[\\[\\s*GOLEM[\\s_-]+HERMES[\\s_-]+" +
			"[A-Z0-9]+([\\s_-]+[A-Z0-9]+)*\\s*\\]\\][`*_~\"'“”‘’]*",
	)
	relayExcessBlankLinesPattern = regexp.MustCompile(`\n{3,}`)
)

const relayInternalTokenOnlyTrimChars = " \t\r\n`*_~\"'“”‘’[]【】()（）<>.,;:!?。；：！？"

func stripRelayInternalTokens(content string) string {
	if !relayInternalTokenPattern.MatchString(content) {
		return content
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	visible := make([]string, 0, len(lines))
	for _, line := range lines {
		lineHadToken := relayInternalTokenPattern.MatchString(line)
		cleaned := relayInternalTokenPattern.ReplaceAllString(line, "")
		if lineHadToken && strings.Trim(cleaned, relayInternalTokenOnlyTrimChars) == "" {
			continue
		}
		visible = append(visible, strings.TrimRight(cleaned, " \t"))
	}
	cleaned := strings.TrimSpace(strings.Join(visible, "\n"))
	return relayExcessBlankLinesPattern.ReplaceAllString(cleaned, "\n\n")
}

func markdownToWeChatText(content string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	result := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			result = append(result, line)
			continue
		}
		if cleaned, keep := cleanMarkdownLine(line); keep {
			result = append(result, cleaned)
		}
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

func cleanMarkdownLine(line string) (string, bool) {
	if markdownRulePattern.MatchString(line) || markdownTableRulePattern.MatchString(line) {
		return "", false
	}
	line = markdownHeadingPattern.ReplaceAllString(line, "")
	line = markdownQuotePattern.ReplaceAllString(line, "")
	line = markdownBulletPattern.ReplaceAllString(line, "- ")
	line = replaceMarkdownLinks(line, markdownImagePattern)
	line = replaceMarkdownLinks(line, markdownLinkPattern)
	line = markdownInlineCode.ReplaceAllString(line, "$1")
	line = markdownBoldAsterisk.ReplaceAllString(line, "$1")
	line = markdownBoldUnderscore.ReplaceAllString(line, "$1")
	line = markdownStrikePattern.ReplaceAllString(line, "$1")
	line = markdownItalicAsterisk.ReplaceAllString(line, "$1")
	line = markdownEscapePattern.ReplaceAllString(line, "$1")
	return html.UnescapeString(line), true
}

func replaceMarkdownLinks(value string, pattern *regexp.Regexp) string {
	return pattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		label := strings.TrimSpace(parts[1])
		url := strings.TrimSpace(parts[2])
		if label == "" || label == url {
			return url
		}
		return label + " (" + url + ")"
	})
}

func canonicalInternalToken(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "[") || !strings.Contains(value, "]") {
		return "", false
	}
	value = strings.Trim(value, " \t\r\n`*~\"'“”‘’[]()（）<>.,;:!?。；：！？")
	parts := strings.FieldsFunc(value, func(char rune) bool {
		return char == '_' || char == '-' || unicode.IsSpace(char)
	})
	if len(parts) == 0 {
		return "", false
	}
	canonical := strings.ToUpper(strings.Join(parts, "_"))
	for _, char := range canonical {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return "", false
		}
	}
	return canonical, true
}
