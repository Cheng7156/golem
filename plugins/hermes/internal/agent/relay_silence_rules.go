package agent

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxSilenceRulesFileBytes = 64 << 10
	maxSilenceRules          = 256
	maxSilenceRuleRunes      = 1000
)

type silenceRule struct {
	kind  string
	value string
}

func (g *RelayGateway) isObserveResponse(content string) bool {
	if isObserveResponse(content) {
		return true
	}
	if g.config.SilenceRulesFile == "" {
		return false
	}
	matched, err := matchesSilenceRulesFile(g.config.SilenceRulesFile, content)
	if err != nil {
		slog.Warn("[hermes] silence rules reload failed",
			"path", g.config.SilenceRulesFile,
			"err", err,
		)
		return false
	}
	return matched
}

func validateSilenceRulesFile(path string) error {
	_, err := loadSilenceRules(path)
	if err != nil {
		return fmt.Errorf("load Hermes silence rules: %w", err)
	}
	return nil
}

func matchesSilenceRulesFile(path string, content string) (bool, error) {
	rules, err := loadSilenceRules(path)
	if err != nil {
		return false, err
	}
	value := strings.ToLower(strings.TrimSpace(unwrapHermesPlainTextFallback(content)))
	for _, rule := range rules {
		if rule.matches(value) {
			return true, nil
		}
	}
	return false, nil
}

func loadSilenceRules(path string) ([]silenceRule, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("rules path is not a regular file")
	}
	if info.Size() > maxSilenceRulesFileBytes {
		return nil, fmt.Errorf("rules file exceeds %d bytes", maxSilenceRulesFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return scanSilenceRules(file)
}

func scanSilenceRules(file *os.File) ([]silenceRule, error) {
	var rules []silenceRule
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		rule, ok, err := parseSilenceRule(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if !ok {
			continue
		}
		rules = append(rules, rule)
		if len(rules) > maxSilenceRules {
			return nil, fmt.Errorf("rules file exceeds %d entries", maxSilenceRules)
		}
	}
	return rules, scanner.Err()
}

func parseSilenceRule(line string) (silenceRule, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return silenceRule{}, false, nil
	}
	kind, value, found := strings.Cut(line, ":")
	if !found {
		return silenceRule{}, false, errors.New("expected exact:, prefix:, or suffix:")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.ToLower(strings.TrimSpace(value))
	if kind != "exact" && kind != "prefix" && kind != "suffix" {
		return silenceRule{}, false, errors.New("unknown rule type")
	}
	if value == "" {
		return silenceRule{}, false, errors.New("rule value is empty")
	}
	return silenceRule{kind: kind, value: value}, true, nil
}

func (r silenceRule) matches(value string) bool {
	switch r.kind {
	case "exact":
		return value == r.value
	case "prefix":
		return strings.HasPrefix(value, r.value)
	case "suffix":
		return strings.HasSuffix(value, r.value)
	default:
		return false
	}
}

func normalizeManagedSilenceRule(kind string, value string) (silenceRule, string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.TrimSpace(value)
	if value == "" {
		return silenceRule{}, "", errors.New("silence rule value is empty")
	}
	if strings.ContainsAny(value, "\r\n") {
		return silenceRule{}, "", errors.New("silence rule value must be one line")
	}
	if len([]rune(value)) > maxSilenceRuleRunes {
		return silenceRule{}, "", fmt.Errorf("silence rule value exceeds %d characters", maxSilenceRuleRunes)
	}
	rule, ok, err := parseSilenceRule(kind + ":" + value)
	if err != nil {
		return silenceRule{}, "", err
	}
	if !ok {
		return silenceRule{}, "", errors.New("silence rule is empty")
	}
	return rule, kind + ":" + value, nil
}

func appendSilenceRule(path string, target silenceRule, line string) (bool, error) {
	rules, err := loadSilenceRules(path)
	if err != nil {
		return false, err
	}
	for _, rule := range rules {
		if rule == target {
			return false, nil
		}
	}
	if len(rules) >= maxSilenceRules {
		return false, fmt.Errorf("rules file exceeds %d entries", maxSilenceRules)
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if len(data) != 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, line...)
	data = append(data, '\n')
	if len(data) > maxSilenceRulesFileBytes {
		return false, fmt.Errorf("rules file exceeds %d bytes", maxSilenceRulesFileBytes)
	}
	if err := writeSilenceRulesAtomic(path, data, info.Mode().Perm()); err != nil {
		return false, err
	}
	verified, err := loadSilenceRules(path)
	if err != nil {
		return false, err
	}
	for _, rule := range verified {
		if rule == target {
			return true, nil
		}
	}
	return false, errors.New("silence rule verification failed")
}

func writeSilenceRulesAtomic(path string, data []byte, mode os.FileMode) (err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".silence-rules-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err = temporary.Write(data); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return err
	}
	dir, openErr := os.Open(directory)
	if openErr != nil {
		return openErr
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
