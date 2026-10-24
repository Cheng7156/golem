package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mkRecipe(srcs []string, ops ...string) *remixRecipe {
	return &remixRecipe{Sources: srcs, Ops: ops, Output: "video"}
}

func op(opName string) string { return opName }

// TestValidateRecipeGates 配方硬闸逐条
func TestValidateRecipeGates(t *testing.T) {
	cases := []struct {
		name    string
		r       *remixRecipe
		wantErr bool
	}{
		{"合法变速", mkRecipe([]string{"latest"}, "speed:2"), false},
		{"无素材", mkRecipe(nil, op("reverse")), true},
		{"素材超限", mkRecipe([]string{"a", "b", "c"}, "reverse"), true},
		{"无操作", mkRecipe([]string{"latest"}), true},
		{"操作数超限", mkRecipe([]string{"latest"},
			"reverse", "mirror", "reverse", "mirror",
			"reverse", "mirror", "reverse", "mirror", "loud"), true},
		{"未知操作", mkRecipe([]string{"latest"}, "rm-rf"), true},
		{"speed 越界", mkRecipe([]string{"latest"}, "speed:10"), true},
		{"trim 倒置", mkRecipe([]string{"latest"}, "trim:5-2"), true},
		{"trim 超时长", mkRecipe([]string{"latest"}, "trim:0-90"), true},
		{"字幕超字数", mkRecipe([]string{"latest"}, "text:"+strings.Repeat("哈", 41)), true},
		{"concat 单素材", mkRecipe([]string{"latest"}, "concat"), true},
		{"concat 双素材", mkRecipe([]string{"latest", "@小王"}, "concat"), false},
		{"gif 输出合法", &remixRecipe{Sources: []string{"latest"}, Ops: []string{"trim:0-3"}, Output: "gif"}, false},
		{"非法输出", &remixRecipe{Sources: []string{"latest"}, Ops: []string{"reverse"}, Output: "exe"}, true},
	}
	for _, c := range cases {
		err := validateRecipe(c.r)
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: wantErr=%v got err=%v", c.name, c.wantErr, err)
		}
	}
}

// genTestVideoLong 生成 4 秒测试视频（含音频轨，变速滤镜需要）
func genTestVideoLong(t *testing.T) []byte {
	t.Helper()
	ff := ffmpegBin("ffmpeg")
	if ff == "" {
		t.Skip("ffmpeg 不可用")
	}
	out := t.TempDir() + "\\t4.mp4"
	args := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=320x240:d=4:r=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", out}
	if err := exec.Command(ff, args...).Run(); err != nil {
		t.Fatalf("生成测试视频失败: %v", err)
	}
	return readAllFile(t, out)
}

// TestRemixPipelineOps 真机 ffmpeg：各操作组合都能产出合法成品
func TestRemixPipelineOps(t *testing.T) {
	src := genTestVideoLong(t)
	cases := []struct {
		name string
		r    *remixRecipe
	}{
		{"变速+倒放", mkRecipe([]string{"in"}, "speed:2", "reverse")},
		{"镜像+循环", mkRecipe([]string{"in"}, "mirror", "loop:2")},
		{"剪3秒+字幕", mkRecipe([]string{"in"}, "trim:0-3", "text:笑死:带'引号'@top")},
		{"裁剪+缩放", mkRecipe([]string{"in"}, "crop:0.8", "scale:240")},
	}
	for _, c := range cases {
		c.r.Sources = []string{"in"} // 源由测试直接喂字节
		out, err := remixVideo(src, c.r)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(out) < 5*1024 {
			t.Fatalf("%s: 成品过小 %d", c.name, len(out))
		}
		if !bytes.Contains(out[:min(64, len(out))], []byte{0x66, 0x74, 0x79, 0x70}) {
			// mp4 box 在头部几十字节内（faststart）
			if !bytes.Contains(out[:min(2048, len(out))], []byte("ftyp")) {
				t.Fatalf("%s: 非 mp4 容器", c.name)
			}
		}
	}
}

// TestRemixGif GIF 双通道
func TestRemixGif(t *testing.T) {
	src := genTestVideoLong(t)
	r := &remixRecipe{Sources: []string{"in"}, Ops: []string{"trim:0-2"}, Output: "gif"}
	out, err := remixVideo(src, r)
	if err != nil {
		t.Fatalf("GIF: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("GIF8")) {
		t.Fatal("非 GIF 容器")
	}
}

// TestRemixTwo 双素材拼接
func TestRemixTwo(t *testing.T) {
	src := genTestVideoLong(t)
	out, err := remixTwo(src, src)
	if err != nil {
		t.Fatalf("拼接: %v", err)
	}
	if len(out) < 10*1024 {
		t.Fatalf("拼接成品过小: %d", len(out))
	}
	// 时长应约 8 秒（4+4）
	dur := probeDur(ffmpegBin("ffprobe"), saveTemp(t, out))
	if dur < 6 || dur > 11 {
		t.Fatalf("拼接时长应约 8s, got %.1f", dur)
	}
}

func readAllFile(t *testing.T, p string) []byte {
	t.Helper()
	d, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func saveTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.mp4")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
