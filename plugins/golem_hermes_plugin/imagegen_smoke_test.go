package main

// 真实服务商冒烟测试（生图→轮询→下载→改图→轮询→下载）。
// 凭据经环境变量注入，绝不写入仓库：
//
//	GOLEM_IMG_SMOKE=1  GOLEM_IMG_BASE=https://image.mlgb7.com/v1  GOLEM_IMG_KEY=...
//
// 运行：go test -run TestSmokeImagegen -count=1 -v -timeout 15m
import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSmokeImagegen(t *testing.T) {
	base := os.Getenv("GOLEM_IMG_BASE")
	key := os.Getenv("GOLEM_IMG_KEY")
	if os.Getenv("GOLEM_IMG_SMOKE") == "" || base == "" || key == "" {
		t.Skip("需要 GOLEM_IMG_SMOKE=1 + GOLEM_IMG_BASE + GOLEM_IMG_KEY")
	}
	c := newImagegenClient(base, key)
	dir := t.TempDir()

	// ---- 1. 异步生图 ----
	t0 := time.Now()
	res, err := c.submitAsync("一只戴着墨镜的橘猫在霓虹灯下吃烤串，赛博朋克风格，竖版海报", "1024x1024", fmt.Sprintf("smoke-%d", time.Now().Unix()), "gpt-image-2.5")
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	t.Logf("提交耗时 %v → task_id=%q url=%q b64=%d字节", time.Since(t0), res.TaskID, res.URL, len(res.B64))

	// ---- 2. 轮询 ----
	var artURL string
	if res.ready() {
		artURL = res.URL
	} else {
		deadline := time.Now().Add(5 * time.Minute)
		for {
			if time.Now().After(deadline) {
				t.Fatal("轮询超时")
			}
			status, art, err := c.queryTask(res.TaskID)
			if art == nil {
				art = &imageResult{}
			}
			t.Logf("  查单 status=%s url=%q b64=%d err=%v", status, art.URL, len(art.B64), err)
			if status == "failed" {
				t.Fatalf("任务失败: %v", err)
			}
			if status == "done" && art.ready() {
				if art.URL != "" {
					artURL = art.URL
				} else {
					res.B64 = art.B64 // 直接落 b64，走下载前通道
				}
				break
			}
			time.Sleep(5 * time.Second)
		}
	}
	t.Logf("生图总耗时 %v", time.Since(t0))

	// ---- 3. 下载 ----
	var data []byte
	if artURL != "" {
		data, err = c.download(artURL)
	} else {
		data, err = base64.StdEncoding.DecodeString(res.B64)
	}
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	genPath := filepath.Join(dir, "smoke-gen.png")
	os.WriteFile(genPath, data, 0o644)
	t.Logf("画稿 %d 字节, sniff=%s", len(data), sniffImage(data))
	if sniffImage(data) == "" {
		t.Fatalf("下载内容不是图片: %x", data[:min(32, len(data))])
	}

	// ---- 4. 参考图改图（拿刚生成的画当素材） ----
	t1 := time.Now()
	res2, err := c.editImage("把它改成毕加索立体主义风格", genPath, "auto", fmt.Sprintf("smoke-edit-%d", time.Now().Unix()), "gpt-image-2.5")
	if err != nil {
		t.Fatalf("改图提交失败: %v", err)
	}
	t.Logf("改图提交耗时 %v → task_id=%q url=%q b64=%d字节", time.Since(t1), res2.TaskID, res2.URL, len(res2.B64))

	// ---- 5. 改图轮询 + 下载 ----
	if !res2.ready() {
		if res2.TaskID == "" {
			t.Fatal("改图既无结果也无任务号")
		}
		deadline := time.Now().Add(5 * time.Minute)
		for {
			if time.Now().After(deadline) {
				t.Fatal("改图轮询超时")
			}
			status, art, err := c.queryTask(res2.TaskID)
			if art == nil {
				art = &imageResult{}
			}
			t.Logf("  改图查单 status=%s url=%q b64=%d err=%v", status, art.URL, len(art.B64), err)
			if status == "failed" {
				t.Fatalf("改图任务失败: %v", err)
			}
			if status == "done" && art.ready() {
				res2 = art
				break
			}
			time.Sleep(5 * time.Second)
		}
	}
	var data2 []byte
	if res2.URL != "" {
		data2, err = c.download(res2.URL)
	} else {
		data2, err = base64.StdEncoding.DecodeString(res2.B64)
	}
	if err != nil {
		t.Fatalf("改图下载失败: %v", err)
	}
	t.Logf("改图总耗时 %v, 画稿 %d 字节, sniff=%s", time.Since(t1), len(data2), sniffImage(data2))
	t.Log("SMOKE OK ✅ 全链路（生图+改图）打通")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
