package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 视频元数据提取：ffmpeg 抽真实首帧做封面 + ffprobe 探时长。
// ffmpeg 由 winget 安装（Gyan.FFmpeg），宿主进程的 PATH 可能早于安装生成
// （LookPath 查不到），因此按 winget 包目录 glob 兜底解析。任一环节失败
// 一律回退占位灰图/默认时长——封面是增强，绝不能阻塞发送主链路。
// ============================================================

var (
	ffMu      sync.Mutex
	ffBinPath = map[string]string{}
)

// ffmpegBin 解析 ffmpeg/ffprobe 可执行文件路径（结果缓存）
func ffmpegBin(name string) string {
	ffMu.Lock()
	defer ffMu.Unlock()
	if p, ok := ffBinPath[name]; ok {
		return p
	}
	p := resolveFFBin(name)
	if p != "" {
		ffBinPath[name] = p
	}
	return p
}

func resolveFFBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if home := os.Getenv("LOCALAPPDATA"); home != "" {
		// winget 包目录带版本号，glob 取字典序最大的（即最新版本）
		pattern := filepath.Join(home, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg*", "*", "bin", name+".exe")
		if m, _ := filepath.Glob(pattern); len(m) > 0 {
			return m[len(m)-1]
		}
	}
	return ""
}

// videoThumbProbe 返回封面帧与真实时长；失败回退占位（时长 0 → 调用方兜底）
func videoThumbProbe(video []byte) (thumb []byte, duration uint32) {
	thumb, dur, err := probeVideoMeta(video)
	if err != nil || len(thumb) == 0 {
		if err != nil {
			slog.Debug("[ghp] 视频封面抽取失败，用占位图", "err", err)
		}
		return placeholderThumb(), 0
	}
	return thumb, dur
}

func probeVideoMeta(video []byte) ([]byte, uint32, error) {
	ff := ffmpegBin("ffmpeg")
	fp := ffmpegBin("ffprobe")
	if ff == "" || fp == "" {
		return nil, 0, errors.New("ffmpeg 未安装")
	}
	dir, err := os.MkdirTemp("", "ghpvid")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, video, 0o644); err != nil {
		return nil, 0, err
	}
	// 真实时长
	dur := uint32(0)
	ctxD, cancelD := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelD()
	if outD, err := exec.CommandContext(ctxD, fp, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", in).Output(); err == nil {
		if f, e := strconv.ParseFloat(strings.TrimSpace(string(outD)), 64); e == nil && f > 0 && f < 3600 {
			dur = uint32(f + 0.5)
		}
	}
	// 抽帧：优先第 1 秒（避开可能的首帧黑屏），失败回退第 0 秒
	frame := filepath.Join(dir, "frame.jpg")
	ctxF, cancelF := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancelF()
	args := []string{"-y", "-v", "error", "-ss", "1", "-i", in, "-vframes", "1", "-vf", "scale=320:-2", "-q:v", "5", frame}
	if err := exec.CommandContext(ctxF, ff, args...).Run(); err != nil {
		args[3] = "0"
		ctxF2, cancelF2 := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancelF2()
		if err = exec.CommandContext(ctxF2, ff, args...).Run(); err != nil {
			return nil, dur, err
		}
	}
	thumb, err := os.ReadFile(frame)
	if err != nil {
		return nil, dur, err
	}
	return thumb, dur, nil
}
