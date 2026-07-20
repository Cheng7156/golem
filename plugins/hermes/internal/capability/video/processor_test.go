package video

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const compatibleProbeJSON = `{
  "format":{"duration":"2.4","format_name":"mov,mp4,m4a,3gp"},
  "streams":[
    {"codec_type":"video","codec_name":"h264","width":640,"height":360},
    {"codec_type":"audio","codec_name":"aac"}
  ]
}`

func TestProcessorKeepsCompatibleVideoAndCreatesThumbnail(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "input.mp4")
	if err := os.WriteFile(input, []byte("valid-video"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	runner := &processorTestRunner{probeOutputs: [][]byte{
		[]byte(compatibleProbeJSON), []byte(compatibleProbeJSON),
	}}
	processor := newTestProcessor(t, directory, runner)
	prepared, err := processor.Prepare(context.Background(), DownloadedMedia{Path: input, Size: 11})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Remove()
	if prepared.VideoPath != input || prepared.Duration != 3 {
		t.Fatalf("prepared=%#v", prepared)
	}
	if info, statErr := os.Stat(prepared.ThumbnailPath); statErr != nil || info.Size() == 0 {
		t.Fatalf("thumbnail info=%v error=%v", info, statErr)
	}
}

func TestProcessorTranscodesIncompatibleVideo(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "input.webm")
	_ = os.WriteFile(input, []byte("source"), 0o600)
	incompatible := strings.Replace(compatibleProbeJSON, `"h264"`, `"vp9"`, 1)
	runner := &processorTestRunner{probeOutputs: [][]byte{
		[]byte(incompatible), []byte(compatibleProbeJSON),
	}}
	processor := newTestProcessor(t, directory, runner)
	prepared, err := processor.Prepare(context.Background(), DownloadedMedia{Path: input, Size: 6})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Remove()
	if prepared.VideoPath == input || !runner.transcoded {
		t.Fatalf("prepared=%#v transcoded=%v", prepared, runner.transcoded)
	}
}

func TestProcessorTranscodesCompatibleVideoOverOutputLimit(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "oversized.mp4")
	data := make([]byte, (1<<20)+1)
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	runner := &processorTestRunner{probeOutputs: [][]byte{
		[]byte(compatibleProbeJSON), []byte(compatibleProbeJSON),
	}}
	processor := newTestProcessor(t, directory, runner)
	prepared, err := processor.Prepare(context.Background(), DownloadedMedia{Path: input, Size: int64(len(data))})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Remove()
	if prepared.VideoPath == input || !runner.transcoded {
		t.Fatalf("prepared=%#v transcoded=%v", prepared, runner.transcoded)
	}
}

func TestProcessorRejectsInvalidProbeDuration(t *testing.T) {
	_, err := parseProbeOutput([]byte(`{"format":{"duration":"unknown"}}`))
	if err == nil {
		t.Fatal("parseProbeOutput accepted invalid duration")
	}
}

func TestProcessorFailsStartupWhenFFmpegIsMissing(t *testing.T) {
	_, err := NewProcessor(ProcessorConfig{
		FFmpegPath:       filepath.Join(t.TempDir(), "missing-ffmpeg"),
		FFprobePath:      filepath.Join(t.TempDir(), "missing-ffprobe"),
		WorkingDirectory: t.TempDir(), MaxVideoBytes: 1 << 20, MaxDuration: 60,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "required executable") {
		t.Fatalf("NewProcessor error=%v", err)
	}
}

type processorTestRunner struct {
	probeOutputs [][]byte
	transcoded   bool
}

func (r *processorTestRunner) Run(_ context.Context, spec CommandSpec) (CommandResult, error) {
	if strings.Contains(spec.Name, "ffprobe") {
		if len(r.probeOutputs) == 0 {
			return CommandResult{}, errors.New("unexpected ffprobe call")
		}
		output := r.probeOutputs[0]
		r.probeOutputs = r.probeOutputs[1:]
		return CommandResult{Stdout: output}, nil
	}
	if len(spec.Args) == 0 {
		return CommandResult{}, errors.New("ffmpeg output is missing")
	}
	output := spec.Args[len(spec.Args)-1]
	if !strings.Contains(output, "thumbnail-") {
		r.transcoded = true
	}
	if err := os.WriteFile(output, []byte("generated"), 0o600); err != nil {
		return CommandResult{}, err
	}
	return CommandResult{}, nil
}

func newTestProcessor(t *testing.T, directory string, runner CommandRunner) *Processor {
	t.Helper()
	value, err := NewProcessor(ProcessorConfig{
		FFmpegPath: "ffmpeg", FFprobePath: "ffprobe", WorkingDirectory: directory,
		MaxVideoBytes: 1 << 20, MaxDuration: 300,
	}, runner)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	return value
}
