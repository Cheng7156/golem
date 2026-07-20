package video

import (
	"fmt"
	"html"
	"net/url"
	"strings"
)

const (
	TransformTrim               = "trim"
	TransformMarkdownLinkTarget = "markdown_link_target"
	TransformHTMLUnescape       = "html_unescape"
	TransformURLDecode          = "url_decode"
	TransformMilliseconds       = "milliseconds_to_seconds"
)

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
		case TransformURLDecode:
			value, err = url.QueryUnescape(value)
		case TransformMilliseconds:
			value, err = millisecondsToSeconds(value)
		default:
			err = fmt.Errorf("unsupported video response transform %q", transform)
		}
		if err != nil {
			return "", err
		}
	}
	return value, nil
}

func markdownLinkTarget(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "[") {
		return value, nil
	}
	separator := strings.Index(trimmed, "](")
	if separator < 1 || !strings.HasSuffix(trimmed, ")") {
		return "", fmt.Errorf("malformed Markdown link in video provider response")
	}
	target := strings.TrimSpace(trimmed[separator+2 : len(trimmed)-1])
	if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
		target = strings.TrimSpace(target[1 : len(target)-1])
	} else if index := strings.IndexAny(target, " \t\r\n"); index >= 0 {
		target = target[:index]
	}
	if target == "" {
		return "", fmt.Errorf("empty Markdown link in video provider response")
	}
	return target, nil
}

func millisecondsToSeconds(value string) (string, error) {
	milliseconds, err := parsePositiveFloat(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("invalid millisecond duration: %w", err)
	}
	return fmt.Sprintf("%.3f", milliseconds/1000), nil
}
