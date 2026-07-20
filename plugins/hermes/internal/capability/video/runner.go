package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	command := exec.CommandContext(ctx, spec.Name, spec.Args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		return result, fmt.Errorf("%s failed: %w: %s", filepathBase(spec.Name), err, commandError(stderr.String()))
	}
	return result, nil
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required executable %s is unavailable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("required executable %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("required executable %s is not executable", path)
	}
	return nil
}

func validateProcessorTools(config ProcessorConfig, runner CommandRunner) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, CommandSpec{
		Name: config.FFmpegPath, Args: []string{"-hide_banner", "-encoders"},
	})
	if err != nil {
		return fmt.Errorf("inspect ffmpeg encoders: %w", err)
	}
	encoders := string(result.Stdout) + string(result.Stderr)
	if !strings.Contains(encoders, "libx264") {
		return errors.New("ffmpeg does not provide the required libx264 encoder")
	}
	if _, err := runner.Run(ctx, CommandSpec{
		Name: config.FFprobePath, Args: []string{"-version"},
	}); err != nil {
		return fmt.Errorf("inspect ffprobe: %w", err)
	}
	return nil
}

func commandError(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maximumCommandErrorRunes {
		return string(runes[:maximumCommandErrorRunes]) + "…"
	}
	return value
}

func filepathBase(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}
