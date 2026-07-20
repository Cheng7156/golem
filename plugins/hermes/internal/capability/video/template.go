package video

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	templateTokenPattern = regexp.MustCompile(`\$\{([^{}]+)\}`)
	environmentPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type templateContext struct {
	request   DiscoveryRequest
	lookupEnv func(string) (string, bool)
}

func expandTemplate(value string, context templateContext) (string, []string, error) {
	var expansionErr error
	var secrets []string
	expanded := templateTokenPattern.ReplaceAllStringFunc(value, func(token string) string {
		if expansionErr != nil {
			return ""
		}
		name := templateTokenPattern.FindStringSubmatch(token)[1]
		replacement, secret, err := resolveTemplateToken(name, context)
		if err != nil {
			expansionErr = err
			return ""
		}
		if secret {
			secrets = append(secrets, replacement)
		}
		return replacement
	})
	if expansionErr != nil {
		return "", nil, expansionErr
	}
	if strings.Contains(expanded, "${") {
		return "", nil, errors.New("malformed video provider template variable")
	}
	return expanded, secrets, nil
}

func resolveTemplateToken(name string, context templateContext) (string, bool, error) {
	switch name {
	case "query":
		return context.request.Query, false, nil
	case "category":
		return context.request.Category, false, nil
	case "limit":
		return strconv.Itoa(context.request.Limit), false, nil
	case "page":
		return strconv.Itoa(context.request.Page), false, nil
	}
	if !strings.HasPrefix(name, "env:") {
		return "", false, fmt.Errorf("unsupported video template variable %q", name)
	}
	environmentName := strings.TrimPrefix(name, "env:")
	if !environmentPattern.MatchString(environmentName) {
		return "", false, fmt.Errorf("invalid environment variable name in template: %s", environmentName)
	}
	if context.lookupEnv == nil {
		return "", false, fmt.Errorf("environment variable %s is unavailable", environmentName)
	}
	value, exists := context.lookupEnv(environmentName)
	if !exists || value == "" {
		return "", false, fmt.Errorf("required environment variable %s is not set", environmentName)
	}
	return value, true, nil
}

func redact(value string, secrets []string) string {
	value = strings.TrimSpace(value)
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	value = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value)
	const maximumErrorRunes = 512
	runes := []rune(value)
	if len(runes) > maximumErrorRunes {
		return string(runes[:maximumErrorRunes]) + "…"
	}
	return value
}
