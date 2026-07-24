package sticker

import (
	"strings"
	"unicode"

	"golem_plugin_hermes/internal/domain"
)

const maxLibraryTerms = 128

func normalizeLibraryText(value string) string {
	var builder strings.Builder
	space := true
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSymbol(r) {
			builder.WriteRune(r)
			space = false
			continue
		}
		if !space && builder.Len() > 0 {
			builder.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func librarySearchTerms(value string) []domain.StickerSearchTerm {
	normalized := normalizeLibraryText(value)
	if normalized == "" {
		return nil
	}
	weights := make(map[string]int, maxLibraryTerms)
	addLibraryTerm(weights, "exact:"+normalized, 100)
	compact := strings.ReplaceAll(normalized, " ", "")
	for _, word := range strings.Fields(normalized) {
		addLibraryTerm(weights, "word:"+word, 40)
		addLibraryNGrams(weights, []rune(word))
	}
	if compact != normalized {
		addLibraryNGrams(weights, []rune(compact))
	}
	result := make([]domain.StickerSearchTerm, 0, len(weights))
	for term, weight := range weights {
		result = append(result, domain.StickerSearchTerm{Value: term, Weight: weight})
	}
	return result
}

func addLibraryNGrams(weights map[string]int, runes []rune) {
	for index, r := range runes {
		addLibraryTerm(weights, "rune:"+string(r), 1)
		if index+2 <= len(runes) {
			addLibraryTerm(weights, "bigram:"+string(runes[index:index+2]), 6)
		}
		if index+3 <= len(runes) {
			addLibraryTerm(weights, "trigram:"+string(runes[index:index+3]), 12)
		}
	}
}

func addLibraryTerm(weights map[string]int, term string, weight int) {
	if len(weights) >= maxLibraryTerms {
		if _, exists := weights[term]; !exists {
			return
		}
	}
	if weight > weights[term] {
		weights[term] = weight
	}
}

func libraryQueryRunes(value string) int {
	return len([]rune(strings.ReplaceAll(normalizeLibraryText(value), " ", "")))
}

func minimumLibraryScore(query string) int {
	switch libraryQueryRunes(query) {
	case 0:
		return 1 << 30
	case 1:
		return 1
	case 2:
		return 6
	default:
		return 4
	}
}
