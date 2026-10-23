package main

import (
	"bytes"
	"image/jpeg"
	"os"
	"os/exec"
	"testing"
)

// genTestVideo 用 ffmpeg 生成 2 秒纯红色测试视频（无 ffmpeg 环境跳过）
func genTestVideo(t *testing.T) []byte {
	t.Helper()
	ff := ffmpegBin("ffmpeg")
	if ff == "" {
		t.Skip("ffmpeg 不可用，跳过封面抽取测试")
	}
	out := t.TempDir() + "\\test.mp4"
	if err := exec.Command(ff, "-y", "-v", "error", "-f", "lavfi", "-i", "color=c=red:s=320x240:d=2", "-pix_fmt", "yuv420p", out).Run(); err != nil {
		t.Fatalf("生成测试视频失败: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestVideoThumbProbe 真实视频应抽出红色调封面（区别于深灰占位图）与正确时长
func TestVideoThumbProbe(t *testing.T) {
	video := genTestVideo(t)
	thumb, dur := videoThumbProbe(video)
	if dur < 1 || dur > 4 {
		t.Fatalf("时长应约 2 秒, got %d", dur)
	}
	if len(thumb) < 500 {
		t.Fatalf("封面过小: %d", len(thumb))
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("封面应为合法 JPEG: %v", err)
	}
	if img.Bounds().Dx() == 64 && img.Bounds().Dy() == 64 {
		t.Fatal("不应还是 64x64 占位图")
	}
	r, g, b, _ := img.At(10, 10).RGBA()
	if !(r > g && r > b) {
		t.Fatalf("红色测试帧的封面应偏红: R=%d G=%d B=%d", r>>8, g>>8, b>>8)
	}
}

// TestVideoThumbProbeFallback 非视频字节应安全回退占位图而非报错
func TestVideoThumbProbeFallback(t *testing.T) {
	thumb, dur := videoThumbProbe(bytes.Repeat([]byte{0x00}, 64*1024))
	if len(thumb) == 0 {
		t.Fatal("回退应返回占位图")
	}
	if dur != 0 {
		t.Fatalf("回退时长应为 0, got %d", dur)
	}
}
