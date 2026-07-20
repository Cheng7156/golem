package sticker

import (
	"fmt"
	"html"
	"strings"
)

const (
	TransformTrim               = "trim"
	TransformMarkdownLinkTarget = "markdown_link_target"
	TransformHTMLUnescape       = "html_unescape"
)

func validateTransforms(transforms []string) error {
	for _, transform := range transforms {
		switch strings.ToLower(strings.TrimSpace(transform)) {
		case TransformTrim, TransformMarkdownLinkTarget, TransformHTMLUnescape:
		default:
			return fmt.Errorf("unsupported sticker response transform %q", transform)
		}
	}
	return nil
}

func applyTransforms(value string, transforms []string) (string, error) {
	var err error
	for _, transform := range transforms {
		switch strings.ToLower(strings.TrimSpace(transform)) {
		case TransformTrim:
			value = strings.TrimSpace(value)
		case TransformHTMLUnescape:
			value = html.UnescapeString(value)
		case TransformMarkdownLinkTarget:
			value, err = markdownLinkTarget(value)
			if err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("unsupported sticker response transform %q", transform)
		}
	}
	return value, nil
}

// markdownLinkTarget accepts an ordinary URL unchanged and extracts the target
// from a complete Markdown inline link such as [label](https://example/img.gif).
func markdownLinkTarget(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "[") {
		return value, nil
	}
	separator := strings.Index(trimmed, "](")
	if separator < 1 || !strings.HasSuffix(trimmed, ")") {
		return "", errorsForMalformedMarkdownLink()
	}
	target := strings.TrimSpace(trimmed[separator+2 : len(trimmed)-1])
	if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
		target = strings.TrimSpace(target[1 : len(target)-1])
	} else if index := strings.IndexAny(target, " \t\r\n"); index >= 0 {
		// Markdown permits an optional title after the URL. Quoted or not, the
		// first whitespace-delimited token is the network target.
		target = target[:index]
	}
	if target == "" {
		return "", errorsForMalformedMarkdownLink()
	}
	return target, nil
}

func errorsForMalformedMarkdownLink() error {
	return fmt.Errorf("malformed Markdown link in sticker provider response")
}
