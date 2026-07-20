package video

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
)

func (p *Processor) transcode(ctx context.Context, input string, duration float64) (string, error) {
	bitrates, err := p.bitrates(duration)
	if err != nil {
		return "", err
	}
	output, err := temporaryOutput(p.config.WorkingDirectory, "prepared-*.mp4")
	if err != nil {
		return "", err
	}
	args := transcodeArgs(input, output, bitrates)
	if _, err := p.runner.Run(ctx, CommandSpec{Name: p.config.FFmpegPath, Args: args}); err != nil {
		_ = os.Remove(output)
		return "", err
	}
	return output, nil
}

type bitratePair struct {
	video int
	audio int
}

func (p *Processor) bitrates(duration float64) (bitratePair, error) {
	budgetBits := float64(p.config.MaxVideoBytes*8*transcodeBudgetPercent) / 100
	total := int(math.Floor(budgetBits / duration))
	audio := defaultAudioBitrate
	if total < minimumVideoBitrate+defaultAudioBitrate {
		audio = lowAudioBitrate
	}
	video := total - audio
	if video < minimumVideoBitrate {
		return bitratePair{}, fmt.Errorf("video cannot fit configured byte limit without dropping below minimum bitrate")
	}
	return bitratePair{video: video, audio: audio}, nil
}

func transcodeArgs(input, output string, bitrates bitratePair) []string {
	videoRate := strconv.Itoa(bitrates.video)
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", localMediaProtocols, "-i", input,
		"-map", "0:v:0", "-map", "0:a:0?", "-map_metadata", "-1",
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-b:v", videoRate, "-maxrate", strconv.Itoa(bitrates.video * 6 / 5),
		"-bufsize", strconv.Itoa(bitrates.video * 2),
		"-c:a", "aac", "-b:a", strconv.Itoa(bitrates.audio),
		"-movflags", "+faststart", "-f", "mp4", output,
	}
}

func (p *Processor) thumbnail(ctx context.Context, input string, duration float64) (string, error) {
	output, err := temporaryOutput(p.config.WorkingDirectory, "thumbnail-*.jpg")
	if err != nil {
		return "", err
	}
	seek := math.Min(1, duration/2)
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", localMediaProtocols, "-ss", fmt.Sprintf("%.3f", seek),
		"-i", input, "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-2", thumbnailWidth),
		"-q:v", "3", output,
	}
	if _, err := p.runner.Run(ctx, CommandSpec{Name: p.config.FFmpegPath, Args: args}); err != nil {
		_ = os.Remove(output)
		return "", err
	}
	info, err := os.Stat(output)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(output)
		return "", fmt.Errorf("ffmpeg did not create a thumbnail")
	}
	return output, nil
}

func temporaryOutput(directory, pattern string) (string, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}
