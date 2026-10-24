package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================
// 视频二创工坊：agent 当导演（翻译群友意图为配方），插件当摄影师
// （校验配方 → ffmpeg 流水线 → 交付）。铁律不变：一切硬闸在 Go，
// agent 只产出结构化配方，永远不碰 shell。
// ============================================================

const (
	rwMaxOps        = 8    // 单配方操作数上限
	rwMaxOutSec     = 60   // 成品时长上限（秒）
	rwMaxOutBytes   = 3 * 1024 * 1024 // 成品大小上限（gRPC 通道）
	rwMaxTextRunes  = 40   // 字幕单条字数上限
	rwMaxSources    = 2    // 素材数上限（拼接用）
	rwExecTimeout   = 90 * time.Second
)

// remixOp 一个二创操作（内部结构，由 DSL 解析而来）
type remixOp struct {
	Op      string
	Start   float64
	End     float64
	Factor  float64
	Content string
	Pos     string
}

// remixRecipe 二创配方（ops 为 DSL 字符串，桥侧确定性解析——嵌套 JSON schema
// 在 hermes 工具系统里渲染为空，DSL 对模型最稳）
// DSL：trim:1-5 | speed:2 | reverse | mirror | loop:2 | text:内容@top|bottom|center |
//      concat | mute | loud | crop:0.8 | scale:480
type remixRecipe struct {
	Sources []string `json:"sources"`
	Ops     []string `json:"ops"`
	Output  string   `json:"output"` // video|gif（默认 video）

	parsed []remixOp `json:"-"` // validateRecipe 填充
}

// parseRemixOps DSL → 操作结构（不合法整条拒绝）
func parseRemixOps(ops []string) ([]remixOp, error) {
	out := make([]remixOp, 0, len(ops))
	for _, raw := range ops {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		name, arg := s, ""
		if i := strings.Index(s, ":"); i > 0 {
			name, arg = s[:i], strings.TrimSpace(s[i+1:])
		}
		op := remixOp{Op: name}
		switch name {
		case "trim":
			var st, en float64
			if n, _ := fmt.Sscanf(arg, "%f-%f", &st, &en); n != 2 {
				return nil, fmt.Errorf("trim 需 trim:起-止，如 trim:1-5，得 %q", raw)
			}
			op.Start, op.End = st, en
		case "speed":
			if _, err := fmt.Sscanf(arg, "%f", &op.Factor); err != nil {
				return nil, fmt.Errorf("speed 需 speed:系数，如 speed:2")
			}
		case "loop":
			if _, err := fmt.Sscanf(arg, "%f", &op.Factor); err != nil {
				return nil, fmt.Errorf("loop 需 loop:次数，如 loop:2")
			}
		case "crop":
			if _, err := fmt.Sscanf(arg, "%f", &op.Factor); err != nil {
				return nil, fmt.Errorf("crop 需 crop:比例，如 crop:0.8")
			}
		case "scale":
			if _, err := fmt.Sscanf(arg, "%f", &op.Factor); err != nil {
				return nil, fmt.Errorf("scale 需 scale:宽度，如 scale:480")
			}
		case "text":
			op.Pos = "bottom"
			if i := strings.LastIndex(arg, "@"); i > 0 {
				p := strings.ToLower(arg[i+1:])
				if p == "top" || p == "bottom" || p == "center" {
					op.Pos = p
					arg = strings.TrimSpace(arg[:i])
				}
			}
			op.Content = arg
		case "reverse", "mirror", "concat", "mute", "loud":
			// 无参操作
		default:
			return nil, fmt.Errorf("不支持的操作: %q", name)
		}
		out = append(out, op)
	}
	return out, nil
}

var remixOpNames = map[string]bool{
	"trim": true, "speed": true, "reverse": true, "mirror": true, "loop": true,
	"text": true, "concat": true, "mute": true, "loud": true, "crop": true, "scale": true,
}

// validateRecipe 配方硬闸：白名单操作、数值边界、字数、操作数
func validateRecipe(r *remixRecipe) error {
	if r == nil {
		return errors.New("配方不能为空")
	}
	if len(r.Sources) == 0 || len(r.Sources) > rwMaxSources {
		return fmt.Errorf("素材数需 1-%d 个", rwMaxSources)
	}
	if len(r.Ops) == 0 {
		return errors.New("至少需要一个操作")
	}
	ops, err := parseRemixOps(r.Ops)
	if err != nil {
		return err
	}
	r.parsed = ops
	if len(r.Ops) > rwMaxOps {
		return fmt.Errorf("操作数超上限（%d > %d）", len(r.Ops), rwMaxOps)
	}
	if r.Output == "" {
		r.Output = "video"
	}
	if r.Output != "video" && r.Output != "gif" {
		return errors.New("output 仅支持 video|gif")
	}
	hasConcat := false
	for _, op := range r.parsed {
		if !remixOpNames[op.Op] {
			return fmt.Errorf("不支持的操作: %s", op.Op)
		}
		switch op.Op {
		case "trim":
			if op.Start < 0 || op.End <= op.Start || op.End > 600 {
				return fmt.Errorf("trim 区间不合法: %.1f-%.1f", op.Start, op.End)
			}
			if op.End-op.Start > rwMaxOutSec {
				return fmt.Errorf("trim 时长超上限 %ds", rwMaxOutSec)
			}
		case "speed":
			if op.Factor < 0.25 || op.Factor > 4 {
				return errors.New("speed 系数需在 0.25-4")
			}
		case "loop":
			if op.Factor < 1 || op.Factor > 3 {
				return errors.New("loop 次数需 1-3")
			}
		case "text":
			n := len([]rune(op.Content))
			if n == 0 || n > rwMaxTextRunes {
				return fmt.Errorf("字幕需 1-%d 字", rwMaxTextRunes)
			}
			if op.Pos == "" {
				op.Pos = "bottom"
			}
			if op.Pos != "top" && op.Pos != "bottom" && op.Pos != "center" {
				return errors.New("text pos 仅 top|bottom|center")
			}
		case "crop":
			if op.Factor < 0.5 || op.Factor > 1 {
				return errors.New("crop 需 0.5-1（中心裁剪比例）")
			}
		case "scale":
			if op.Factor < 144 || op.Factor > 1080 {
				return errors.New("scale 宽度需 144-1080")
			}
		case "concat":
			hasConcat = true
		}
	}
	if hasConcat && len(r.Sources) < 2 {
		return errors.New("concat 需要两个素材")
	}
	return nil
}

// buildFilterChain 操作序列 → 视频滤镜链 + 音频滤镜链 + 输入侧参数。
// 视频/音频必须分链（atempo/volume 是音频滤镜，混进 -vf 直接报错）；
// text 除外：走 SRT 烧录防转义地狱。
func buildFilterChain(r *remixRecipe, srtPath string) (vf, af string, extraIn []string, err error) {
	var vchain, achain []string
	for _, op := range r.parsed {
		switch op.Op {
		case "trim":
			// trim 放 -ss/-to 输入侧更稳（不用重解码整个文件）
			extraIn = append(extraIn, "-ss", fmt.Sprintf("%.2f", op.Start), "-to", fmt.Sprintf("%.2f", op.End))
		case "speed":
			vchain = append(vchain, fmt.Sprintf("setpts=%.4f*PTS", 1/op.Factor))
			achain = append(achain, fmt.Sprintf("atempo=%.4f", op.Factor))
		case "reverse":
			vchain = append(vchain, "reverse")
		case "mirror":
			vchain = append(vchain, "hflip")
		case "loop":
			if op.Factor > 1 {
				vchain = append(vchain, fmt.Sprintf("loop=loop=%d:size=1", int(op.Factor)-1))
			}
		case "crop":
			vchain = append(vchain, fmt.Sprintf("crop=iw*%.2f:ih*%.2f", op.Factor, op.Factor))
		case "scale":
			vchain = append(vchain, fmt.Sprintf("scale=%d:-2", int(op.Factor)))
		case "concat":
			// concat 在上层走 concat demuxer，不进滤镜链
		case "mute":
			extraIn = append(extraIn, "-an")
		case "loud":
			achain = append(achain, "volume=2.0")
		case "text":
			// SRT 烧录在 vf 末尾追加 subtitles 滤镜（见下）
		}
	}
	if srtPath != "" {
		// Windows 路径转义：C\:/...（ffmpeg 滤镜语法）
		esc := strings.ReplaceAll(srtPath, "\\", "/")
		esc = strings.ReplaceAll(esc, ":", "\\:")
		vchain = append(vchain, fmt.Sprintf("subtitles='%s':force_style='FontName=Microsoft YaHei,FontSize=18,Outline=1'", esc))
	}
	return strings.Join(vchain, ","), strings.Join(achain, ","), extraIn, nil
}

// writeSRT 把 text 操作写成 SRT 文件（全时长显示，位置由 Margin 控制）
func writeSRT(dir string, r *remixRecipe, durSec float64) (string, string, error) {
	var b strings.Builder
	idx := 1
	end := durSec
	if end <= 0 {
		end = rwMaxOutSec
	}
	for _, op := range r.parsed {
		if op.Op != "text" {
			continue
		}
		fmt.Fprintf(&b, "%d\n00:00:00,000 --> %02d:%02d:%02d,000\n%s\n\n",
			idx, int(end/3600), int(end/60)%60, int(end)%60, op.Content)
		idx++
	}
	if idx == 1 {
		return "", "", nil
	}
	p := filepath.Join(dir, "cap.srt")
	if err := os.WriteFile(p, []byte("\ufeff"+b.String()), 0o644); err != nil {
		return "", "", err
	}
	// MarginV 按位置：top=30 bottom=40 center=屏高一半
	margin := "40"
	align := "2"
	for _, op := range r.parsed {
		if op.Op == "text" {
			switch op.Pos {
			case "top":
				margin, align = "30", "6"
			case "center":
				margin, align = "0", "5"
			}
			break
		}
	}
	return p, fmt.Sprintf("MarginV=%s,Alignment=%s", margin, align), nil
}

// runRemixCmd 执行一条 ffmpeg 命令（统一超时与错误捕获）
func runRemixCmd(args ...string) error {
	ff := ffmpegBin("ffmpeg")
	if ff == "" {
		return errors.New("ffmpeg 不可用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rwExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ff, args...)
	var eb strings.Builder
	cmd.Stderr = &eb
	if err := cmd.Run(); err != nil {
		tail := eb.String()
		if i := strings.LastIndex(tail, "\n"); i > 0 {
			tail = tail[max(0, i-200):]
		}
		return fmt.Errorf("ffmpeg: %v | %s", err, strings.TrimSpace(tail))
	}
	return nil
}

// remixVideo 主流程：素材字节（已下载/已合并）→ 配方执行 → 成品字节
func remixVideo(input []byte, r *remixRecipe) ([]byte, error) {
	if err := validateRecipe(r); err != nil {
		return nil, err
	}
	ff := ffmpegBin("ffmpeg")
	fp := ffmpegBin("ffprobe")
	if ff == "" || fp == "" {
		return nil, errors.New("ffmpeg 未安装")
	}
	dir, err := os.MkdirTemp("", "ghpremix")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// concat：先把两段各自转统一编码再拼接
	in := filepath.Join(dir, "in.mp4")
	if r.Output == "gif" {
		in = filepath.Join(dir, "in_src.mp4")
	}
	if err := os.WriteFile(in, input, 0o644); err != nil {
		return nil, err
	}
	if len(r.Sources) > 1 || hasOp(r, "concat") {
		if err := runRemixCmd("-y", "-v", "error", "-i", in, "-c:v", "libx264", "-preset", "veryfast", "-c:a", "aac", "-ar", "44100", "-ac", "2", filepath.Join(dir, "a.mp4")); err != nil {
			return nil, fmt.Errorf("素材标准化失败: %w", err)
		}
		// 第二素材由调用方拼在 extraInput（这里只处理已合并单文件的简单链路；
		// 双素材 concat 由 bridge 层下载两条后 concat filter 合一，见 remixTwo）
		in = filepath.Join(dir, "a.mp4")
	}

	dur := probeDur(fp, in)
	srt, style, err := writeSRT(dir, r, dur)
	if err != nil {
		return nil, err
	}
	vf, af, extraIn, _ := buildFilterChain(r, srt)
	out := filepath.Join(dir, "out.mp4")

	if r.Output == "gif" {
		// GIF 双通道：palettegen → paletteuse，宽默认 480 限流；GIF 无音频，-an
		w := 480
		for _, op := range r.parsed {
			if op.Op == "scale" {
				w = int(op.Factor)
			}
		}
		pal := filepath.Join(dir, "pal.png")
		genArgs := append([]string{"-y", "-v", "error"}, extraIn...)
		genArgs = append(genArgs, "-i", in, "-an", "-vf", joinVF(vf, fmt.Sprintf("fps=12,scale=%d:-2:flags=lanczos,palettegen=stats_mode=diff", w)), pal)
		if err := runRemixCmd(genArgs...); err != nil {
			return nil, err
		}
		useArgs := append([]string{"-y", "-v", "error"}, extraIn...)
		graph := fmt.Sprintf("[0:v]fps=12,scale=%d:-2:flags=lanczos [x]; [x][1:v] paletteuse=dither=bayer:bayer_scale=2", w)
		if vf != "" {
			graph = fmt.Sprintf("[0:v]%s,fps=12,scale=%d:-2:flags=lanczos [x]; [x][1:v] paletteuse=dither=bayer:bayer_scale=2", vf, w)
		}
		useArgs = append(useArgs, "-i", in, "-i", pal, "-an", "-lavfi", graph,
			"-loop", "0", filepath.Join(dir, "out.gif"))
		if err := runRemixCmd(useArgs...); err != nil {
			return nil, err
		}
		return os.ReadFile(filepath.Join(dir, "out.gif"))
	}

	args := []string{"-y", "-v", "error"}
	args = append(args, extraIn...)
	args = append(args, "-i", in)
	if vf != "" {
		vf2 := vf
		if style != "" {
			// force_style 追加 Margin/Alignment
			vf2 = strings.Replace(vf, "Outline=1", "Outline=1,"+style, 1)
		}
		args = append(args, "-vf", vf2)
	}
	if af != "" && !hasOp(r, "mute") {
		args = append(args, "-af", af)
	}
	args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", out)
	if err := runRemixCmd(args...); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	// 成品超限：压缩阶梯加压（多级），塞下为止
	if len(data) > rwMaxOutBytes {
		got, cerr := compressVideoTo(data, rwMaxOutBytes)
		if cerr != nil {
			return nil, fmt.Errorf("成品 %.1fMB 超上限且压缩失败: %w", float64(len(data))/1024/1024, cerr)
		}
		return got, nil
	}
	return data, nil
}

// remixTwo 双素材合并：等宽标准化后 concat demuxer 拼接（简单可靠）
func remixTwo(a, b []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "ghpcat")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for i, src := range [][]byte{a, b} {
		in := filepath.Join(dir, fmt.Sprintf("s%d.mp4", i))
		if err := os.WriteFile(in, src, 0o644); err != nil {
			return nil, err
		}
		// 每段先钳到 ≤30s，再统一 480 宽/30fps/aac（concat 的硬前提）
		norm := filepath.Join(dir, fmt.Sprintf("n%d.mp4", i))
		if err := runRemixCmd("-y", "-v", "error", "-i", in, "-t", "30",
			"-vf", "scale=480:-2:flags=lanczos,fps=30,format=yuv420p",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "26",
			"-c:a", "aac", "-ar", "44100", "-ac", "2", norm); err != nil {
			return nil, fmt.Errorf("素材 %d 标准化失败: %w", i+1, err)
		}
	}
	list := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(list, []byte(fmt.Sprintf("file '%s'\nfile '%s'\n",
		filepath.Join(dir, "n0.mp4"), filepath.Join(dir, "n1.mp4"))), 0o644); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "cat.mp4")
	if err := runRemixCmd("-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", list, "-c", "copy", out); err != nil {
		return nil, fmt.Errorf("拼接失败: %w", err)
	}
	return os.ReadFile(out)
}

// ---- 小工具 ----

func hasOp(r *remixRecipe, name string) bool {
	for _, op := range r.parsed {
		if op.Op == name {
			return true
		}
	}
	return false
}

func probeDur(fp, path string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, fp, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	var f float64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &f)
	return f
}

func joinVF(base, extra string) string {
	if base == "" {
		return extra
	}
	return base + "," + extra
}
