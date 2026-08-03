package main

import (
	"regexp"
	"strings"
)

var (
	markdownImagePattern  = regexp.MustCompile(`!\[([^]\n]*)\]\(([^)\n]+)\)`)
	markdownLinkPattern   = regexp.MustCompile(`\[([^]\n]+)\]\(([^)\n]+)\)`)
	markdownStrongStar    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	markdownStrongUnder   = regexp.MustCompile(`__([^_\n]+)__`)
	markdownStrikePattern = regexp.MustCompile(`~~([^~\n]+)~~`)
	markdownCodePattern   = regexp.MustCompile("`([^`\\n]+)`")
	markdownEmphasisStar  = regexp.MustCompile(`\*([^*\s](?:[^*\n]*[^*\s])?)\*`)
	markdownAutoLink      = regexp.MustCompile(`<((?:https?://|mailto:)[^>\n]+)>`)
)

func prepareOutboundText(outputs []outbound) []outbound {
	prepared := make([]outbound, 0, len(outputs))
	for _, output := range outputs {
		if output.Kind != "text" {
			prepared = append(prepared, output)
			continue
		}

		output.Text = markdownToPlainText(output.Text)
		if output.Text == "" {
			continue
		}
		if len(prepared) > 0 && prepared[len(prepared)-1].Kind == "text" {
			prepared[len(prepared)-1].Text += "\n" + output.Text
			continue
		}
		prepared = append(prepared, output)
	}
	return prepared
}

func markdownToPlainText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	plainLines := make([]string, 0, len(lines))
	inFence := false

	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence {
			trimmed = stripMarkdownLinePrefix(trimmed)
			if isMarkdownRule(trimmed) {
				continue
			}
		}
		plainLines = append(plainLines, strings.TrimRight(trimmed, " \t"))
	}

	value = strings.Join(plainLines, "\n")
	value = markdownImagePattern.ReplaceAllString(value, "$1 ($2)")
	value = markdownLinkPattern.ReplaceAllString(value, "$1 ($2)")
	value = markdownStrongStar.ReplaceAllString(value, "$1")
	value = markdownStrongUnder.ReplaceAllString(value, "$1")
	value = markdownStrikePattern.ReplaceAllString(value, "$1")
	value = markdownCodePattern.ReplaceAllString(value, "$1")
	value = markdownEmphasisStar.ReplaceAllString(value, "$1")
	value = markdownAutoLink.ReplaceAllString(value, "$1")
	for _, escaped := range []string{"*", "_", "#", "[", "]", "`", "~", ">"} {
		value = strings.ReplaceAll(value, "\\"+escaped, escaped)
	}
	for strings.Contains(value, "\n\n\n") {
		value = strings.ReplaceAll(value, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(value)
}

func stripMarkdownLinePrefix(line string) string {
	for strings.HasPrefix(line, ">") {
		line = strings.TrimLeft(strings.TrimPrefix(line, ">"), " \t")
	}

	if strings.HasPrefix(line, "#") {
		heading := strings.TrimLeft(line, "#")
		if heading != line && len(heading) > 0 && (heading[0] == ' ' || heading[0] == '\t') {
			line = strings.TrimLeft(heading, " \t")
		}
	}

	for _, marker := range []string{"- [ ] ", "* [ ] ", "+ [ ] "} {
		if strings.HasPrefix(line, marker) {
			return "☐ " + strings.TrimPrefix(line, marker)
		}
	}
	for _, marker := range []string{"- [x] ", "- [X] ", "* [x] ", "* [X] ", "+ [x] ", "+ [X] "} {
		if strings.HasPrefix(line, marker) {
			return "☑ " + strings.TrimPrefix(line, marker)
		}
	}
	if len(line) >= 2 && strings.ContainsRune("-*+", rune(line[0])) && (line[1] == ' ' || line[1] == '\t') {
		return "• " + strings.TrimLeft(line[1:], " \t")
	}
	return line
}

func isMarkdownRule(line string) bool {
	compact := strings.ReplaceAll(strings.ReplaceAll(line, " ", ""), "\t", "")
	if len(compact) < 3 {
		return false
	}
	return strings.Trim(compact, "-") == "" ||
		strings.Trim(compact, "*") == "" ||
		strings.Trim(compact, "_") == ""
}
