package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

type probeDocument struct {
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
}

func (p *Processor) probe(ctx context.Context, path string) (MediaInfo, error) {
	result, err := p.runner.Run(ctx, CommandSpec{
		Name: p.config.FFprobePath,
		Args: []string{
			"-v", "error", "-protocol_whitelist", localMediaProtocols, "-show_entries",
			"format=duration,format_name:stream=codec_type,codec_name,width,height",
			"-of", "json", path,
		},
	})
	if err != nil {
		return MediaInfo{}, err
	}
	return parseProbeOutput(result.Stdout)
}

func parseProbeOutput(value []byte) (MediaInfo, error) {
	var document probeDocument
	if err := json.Unmarshal(value, &document); err != nil {
		return MediaInfo{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	duration, err := strconv.ParseFloat(document.Format.Duration, 64)
	if err != nil {
		return MediaInfo{}, errors.New("ffprobe returned an invalid duration")
	}
	info := MediaInfo{Duration: duration, Format: document.Format.FormatName}
	for _, stream := range document.Streams {
		switch stream.CodecType {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec, info.Width, info.Height = stream.CodecName, stream.Width, stream.Height
			}
		case "audio":
			if info.AudioCodec == "" {
				info.AudioCodec = stream.CodecName
			}
		}
	}
	return info, nil
}
