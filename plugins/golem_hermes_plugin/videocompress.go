package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ============================================================
// 视频压缩：超 gRPC 上限（3MB）的视频不再拒收，走压缩阶梯重编码。
// 阶梯逐级加压（缩分辨率→提 CRF→降帧率→砍音频码率），任一级达标即停；
// 全梯走完仍超才报错。硬顶 rwMaxDownloadBytes（50MB）防拉超大文件占满磁盘。
// ============================================================

const rwMaxDownloadBytes = 50 * 1024 * 1024

// compressLadder 压缩阶梯：越靠后越狠
var compressLadder = []struct {
	scale   int
	crf     int
	fps     int
	audioK  int
}{
	{480, 28, 30, 96},
	{360, 30, 24, 64},
	{240, 32, 20, 48},
	{240, 34, 15, 32},
	{180, 36, 12, 24},
}

// compressVideoTo 把视频压到 ≤ targetBytes；srcDur 用于校验成品时长没被截断
func compressVideoTo(data []byte, targetBytes int) ([]byte, error) {
	ff := ffmpegBin("ffmpeg")
	fp := ffmpegBin("ffprobe")
	if ff == "" || fp == "" {
		return nil, errors.New("ffmpeg 不可用，无法压缩")
	}
	dir, err := os.MkdirTemp("", "ghpcompress")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, data, 0o644); err != nil {
		return nil, err
	}
	srcDur := probeDur(fp, in)
	var lastErr error
	for i, step := range compressLadder {
		out := filepath.Join(dir, fmt.Sprintf("c%d.mp4", i))
		args := []string{"-y", "-v", "error", "-i", in,
			"-vf", fmt.Sprintf("scale=%d:-2:flags=lanczos,fps=%d", step.scale, step.fps),
			"-c:v", "libx264", "-preset", "veryfast", "-crf", fmt.Sprintf("%d", step.crf),
			"-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", step.audioK), "-ac", "1",
			"-movflags", "+faststart", out}
		if err := runRemixCmd(args...); err != nil {
			lastErr = err
			continue // 本级失败试下一级（更狠的参数有时反而能过）
		}
		got, err := os.ReadFile(out)
		if err != nil {
			lastErr = err
			continue
		}
		if len(got) <= targetBytes {
			// 时长完整性：成品时长不应低于源的 80%（防编码事故产出残片）
			if srcDur > 0 {
				outDur := probeDur(fp, out)
				if outDur < srcDur*0.8 {
					lastErr = fmt.Errorf("第 %d 级压缩时长异常 %.1fs<%.1fs", i, outDur, srcDur)
					continue
				}
			}
			return got, nil
		}
		lastErr = fmt.Errorf("第 %d 级压缩后仍 %.1fMB", i, float64(len(got))/1024/1024)
	}
	return nil, fmt.Errorf("压缩阶梯全部失败: %v", lastErr)
}

// ensureSendable 统一入口：≤ limit 原样返回，超限自动压缩（压缩失败才报错）
func ensureSendable(data []byte, limit int) ([]byte, bool, error) {
	if len(data) <= limit {
		return data, false, nil
	}
	out, err := compressVideoTo(data, limit)
	if err != nil {
		return nil, false, fmt.Errorf("视频 %.1fMB 超限且压缩失败: %w", float64(len(data))/1024/1024, err)
	}
	return out, true, nil
}

// humanSize 供日志
func humanSize(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/1024/1024)
	case n >= 1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
