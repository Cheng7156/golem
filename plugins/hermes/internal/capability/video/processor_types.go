package video

import (
	"context"
	"os"
)

const (
	maximumOutputWidth       = 1280
	maximumOutputHeight      = 720
	thumbnailWidth           = 320
	minimumVideoBitrate      = 200_000
	defaultAudioBitrate      = 96_000
	lowAudioBitrate          = 64_000
	transcodeBudgetPercent   = 92
	maximumCommandErrorRunes = 4096
	localMediaProtocols      = "file,pipe"
)

type ProcessorConfig struct {
	FFmpegPath       string
	FFprobePath      string
	WorkingDirectory string
	MaxVideoBytes    int64
	MaxDuration      uint32
}

type CommandSpec struct {
	Name string
	Args []string
}

type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

type CommandRunner interface {
	Run(context.Context, CommandSpec) (CommandResult, error)
}

type MediaInfo struct {
	Duration   float64
	Format     string
	VideoCodec string
	AudioCodec string
	Width      int
	Height     int
}

type PreparedMedia struct {
	VideoPath     string
	ThumbnailPath string
	Duration      uint32
}

func (m PreparedMedia) Remove() error {
	var first error
	for _, path := range []string{m.VideoPath, m.ThumbnailPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}
