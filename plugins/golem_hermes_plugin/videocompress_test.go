package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCompressVideoToLadder 真实压缩：高码率视频压进 1MB 且时长保留
func TestCompressVideoToLadder(t *testing.T) {
	ff := ffmpegBin("ffmpeg")
	if ff == "" {
		t.Skip("ffmpeg 不可用")
	}
	// 6 秒 mandelbrot 视频（细节密集难压缩，保证 >1MB）
	out := filepath.Join(t.TempDir(), "big.mp4")
	if err := exec.Command(ff, "-y", "-v", "error",
		"-f", "lavfi", "-i", "mandelbrot=s=640x480:rate=30", "-t", "6",
		"-f", "lavfi", "-i", "sine=frequency=600:duration=6",
		"-c:v", "libx264", "-crf", "18", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", out).Run(); err != nil {
		t.Fatalf("生成高码率视频失败: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 1024*1024 {
		t.Fatalf("测试前提失败：源视频应 >1MB, got %d", len(data))
	}
	got, err := compressVideoTo(data, 1024*1024)
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if len(got) > 1024*1024 {
		t.Fatalf("压缩后仍超限: %d", len(got))
	}
	dur := probeDur(ffmpegBin("ffprobe"), saveTemp(t, got))
	if dur < 4.5 || dur > 8 {
		t.Fatalf("压缩后时长应保留约 6s, got %.1f", dur)
	}
}

// TestEnsureSendable 不超限原样直通
func TestEnsureSendable(t *testing.T) {
	small := []byte("tiny")
	got, compressed, err := ensureSendable(small, 1024)
	if err != nil || compressed || string(got) != "tiny" {
		t.Fatalf("小文件应直通: %v %v", compressed, err)
	}
}
