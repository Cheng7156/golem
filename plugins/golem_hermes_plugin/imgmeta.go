package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/sbgayhub/golem/sdk/message"
)

// ============================================================
// 图片元数据抢救：宿主 buildImage 按旧报文格式解析 <img> 属性，当前客户端
// 报文下全部解析为零值 → Content 渲染成 "[0 x 0] 0B"（模型照念说 0B）、
// Media 三元组为空 → vision 下载失败、画坊改图无素材（image_ring 恒空）。
// SDK Message.Raw 里存着宿主转存的 NewMessage JSON（含原始 content XML），
// 插件在这里自解析，属性名做宽集合匹配，不赌单一格式。
// ============================================================

type rawImageMeta struct {
	Md5    string
	Key    string
	Url    string
	Size   uint32
	Width  uint32
	Height uint32
}

var (
	imgTagRe       = regexp.MustCompile(`(?is)<img\s[^>]*/?>`)
	imgAttrRe      = regexp.MustCompile(`([A-Za-z0-9_]+)\s*=\s*"((?:[^"\\]|\\.)*)"`)
	imgPrefixRe    = regexp.MustCompile(`^[^\s<]{1,80}:\n`) // 群消息 "wxid:\n" 前缀
	xmlEscapeField = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'")
)

// attrOf 从属性宽集合里按优先级取第一个非空值
func attrOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[strings.ToLower(k)]; v != "" {
			return v
		}
	}
	return ""
}

// attrU32 同上并转 uint32
func attrU32(m map[string]string, keys ...string) uint32 {
	var v uint64
	for _, k := range keys {
		s := m[strings.ToLower(k)]
		if s == "" {
			continue
		}
		if n, err := fmt.Sscanf(s, "%d", &v); err == nil && n == 1 && v > 0 {
			return uint32(v)
		}
	}
	return 0
}

// parseRawImage 从消息 Raw 报文中抢救图片元数据；拿不到返回 false
func parseRawImage(msg *message.Message) (rawImageMeta, bool) {
	meta := rawImageMeta{}
	if msg == nil {
		return meta, false
	}
	raw := msg.GetRaw()
	if raw == "" {
		return meta, false
	}
	var nm struct {
		Content struct {
			Value string `json:"value"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &nm); err != nil || nm.Content.Value == "" {
		slog.Info("[ghp] 图片Raw无content", "raw_len", len(raw))
		return meta, false
	}
	xmlStr := imgPrefixRe.ReplaceAllString(nm.Content.Value, "")
	xmlStr = strings.ReplaceAll(xmlStr, "\\/", "/")
	tag := imgTagRe.FindString(xmlStr)
	if tag == "" {
		slog.Info("[ghp] 图片Raw无img标签", "head", previewOf(xmlStr, 160))
		return meta, false
	}
	attrs := map[string]string{}
	for _, sm := range imgAttrRe.FindAllStringSubmatch(tag, -1) {
		attrs[strings.ToLower(sm[1])] = xmlEscapeField.Replace(sm[2])
	}
	meta.Md5 = attrOf(attrs, "md5")
	meta.Key = attrOf(attrs, "aeskey", "cdnthumbaeskey", "encryhdkey")
	meta.Url = attrOf(attrs, "cdnmidimgurl", "cdnthumburl", "cdnhdimgurl", "url")
	meta.Size = attrU32(attrs, "length", "cdnthumblength", "hevc_mid_size", "datalen", "size")
	meta.Width = attrU32(attrs, "cdnthumbwidth", "width", "w")
	meta.Height = attrU32(attrs, "cdnthumbheight", "height", "h")
	// 首次对样：把真实 img 标签留在日志里，属性名有变一眼可见
	slog.Info("[ghp] 图片元数据抢救", "md5", previewOf(meta.Md5, 12), "size", meta.Size,
		"dim", fmt.Sprintf("%dx%d", meta.Width, meta.Height), "tag", previewOf(tag, 300))
	return meta, meta.Md5 != "" || meta.Size > 0
}

// imageContextLabel 群上下文/归档用的图片摘要：能抢救出真实信息就展示，
// 抢救不出也不渲染误导性的 "[0 x 0] 0B"（模型会把 0B 当事实转述）
func imageContextLabel(msg *message.Message) string {
	if meta, ok := parseRawImage(msg); ok && (meta.Width > 0 || meta.Size > 0) {
		dim := ""
		if meta.Width > 0 && meta.Height > 0 {
			dim = fmt.Sprintf("%dx%d ", meta.Width, meta.Height)
		}
		return fmt.Sprintf("[图片 %s%s]", dim, humanBytes(meta.Size))
	}
	return "[图片]"
}

func humanBytes(n uint32) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dKB", n/1024)
	case n > 0:
		return fmt.Sprintf("%dB", n)
	default:
		return ""
	}
}

// imageMediaOrRaw 返回消息自带 Media；三元组为空（宿主解析零值的现状）则从 Raw 抢救
func imageMediaOrRaw(msg *message.Message) (md5, key, url string, size uint32, ok bool) {
	if md := msg.GetImage().GetMedia(); md != nil && md.GetMd5() != "" {
		return md.GetMd5(), md.GetKey(), md.GetUrl(), md.GetSize(), true
	}
	meta, ok := parseRawImage(msg)
	if !ok || meta.Md5 == "" {
		return "", "", "", 0, false
	}
	return meta.Md5, meta.Key, meta.Url, meta.Size, true
}
