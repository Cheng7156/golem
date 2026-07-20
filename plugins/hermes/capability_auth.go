package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golem_plugin_hermes/internal/config"
)

type capabilityEnvironment map[string]string

const minimumCapabilityTokenLength = 16

func loadCapabilityEnvironment(path string) (capabilityEnvironment, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Hermes capability environment file: %w", err)
	}
	defer file.Close()
	values := make(capabilityEnvironment)
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		name, value, skip, parseErr := parseCapabilityEnvironmentLine(scanner.Text())
		if parseErr != nil {
			return nil, fmt.Errorf("parse Hermes capability environment file line %d: %w", lineNumber, parseErr)
		}
		if !skip {
			values[name] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Hermes capability environment file: %w", err)
	}
	return values, nil
}

func parseCapabilityEnvironmentLine(line string) (string, string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", true, nil
	}
	name, value, found := strings.Cut(line, "=")
	name = strings.TrimSpace(name)
	if !found || !validEnvironmentName(name) {
		return "", "", false, errors.New("expected NAME=VALUE")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false, fmt.Errorf("%s is empty", name)
	}
	return name, value, false, nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func capabilityEnvironmentLookup(values capabilityEnvironment) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if value, exists := values[name]; exists {
			return value, true
		}
		return os.LookupEnv(name)
	}
}

func resolveCapabilityToken(
	value config.CapabilityConfig,
	lookup func(string) (string, bool),
) (string, error) {
	token, _ := lookup(value.SharedTokenEnv)
	token = strings.TrimSpace(token)
	if len(token) < minimumCapabilityTokenLength {
		return "", fmt.Errorf(
			"Hermes capability environment %s must contain at least 16 characters",
			value.SharedTokenEnv,
		)
	}
	return token, nil
}
