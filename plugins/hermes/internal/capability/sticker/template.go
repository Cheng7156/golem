package sticker

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	templateToken = regexp.MustCompile(`\$\{([^{}]+)\}`)
	envName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type templateValues struct {
	query     string
	limit     int
	page      int
	lookupEnv func(string) (string, bool)
}

// expandTemplate returns expanded text and the environment-derived values so
// callers can redact credentials from remote error messages.
func expandTemplate(value string, values templateValues) (string, []string, error) {
	var expansionErr error
	var secrets []string
	expanded := templateToken.ReplaceAllStringFunc(value, func(token string) string {
		if expansionErr != nil {
			return ""
		}
		name := templateToken.FindStringSubmatch(token)[1]
		switch name {
		case "query":
			return values.query
		case "limit":
			return strconv.Itoa(values.limit)
		case "page":
			return strconv.Itoa(values.page)
		}
		if strings.HasPrefix(name, "env:") {
			environmentName := strings.TrimPrefix(name, "env:")
			if !envName.MatchString(environmentName) {
				expansionErr = fmt.Errorf("invalid environment variable name in template: %s", environmentName)
				return ""
			}
			if values.lookupEnv == nil {
				expansionErr = fmt.Errorf("environment variable %s is unavailable", environmentName)
				return ""
			}
			environmentValue, exists := values.lookupEnv(environmentName)
			if !exists || environmentValue == "" {
				expansionErr = fmt.Errorf("required environment variable %s is not set", environmentName)
				return ""
			}
			secrets = append(secrets, environmentValue)
			return environmentValue
		}
		expansionErr = fmt.Errorf("unsupported sticker template variable %q", name)
		return ""
	})
	if expansionErr != nil {
		return "", nil, expansionErr
	}
	if strings.Contains(expanded, "${") {
		return "", nil, errors.New("malformed sticker template variable")
	}
	return expanded, secrets, nil
}

func validateTemplate(value string) error {
	_, _, err := expandTemplate(value, templateValues{
		query: "query",
		limit: 1,
		page:  1,
		lookupEnv: func(string) (string, bool) {
			return "secret", true
		},
	})
	return err
}

func redact(value string, secrets []string) string {
	value = strings.TrimSpace(value)
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	value = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value)
	const maximumRunes = 512
	runes := []rune(value)
	if len(runes) > maximumRunes {
		value = string(runes[:maximumRunes]) + "…"
	}
	return value
}
