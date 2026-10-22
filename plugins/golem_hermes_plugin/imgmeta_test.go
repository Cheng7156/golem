package main

import (
	"strings"
	"testing"

	"github.com/sbgayhub/golem/sdk/message"
)

// mkImageMsg 构造带 Raw 报文的图片消息（宿主 Build 转存 NewMessage JSON 的现状）
func mkImageMsg(content string) *message.Message {
	raw := `{"id":1,"type":3,"content":{"value":` + jsonQuote(content) + `},"new_id":42}`
	return &message.Message{
		Type:    message.TypeImage,
		Content: "[0 x 0] 0B", // 宿主对当前报文的零值摘要
		Raw:     raw,
	}
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

const wxImgXML = `wxid_abc:\n<msg><img aeskey="aesk_123" encryhdkey="" cdnthumbaeskey="taesk" ` +
	`cdnthumburl="3057020100044b3049..." cdnmidimgurl="3057020100044bmid..." cdnhdimgurl="" ` +
	`length="88042" crc32="2138585446" md5="md5abc123" hevc_mid_size="66666" ` +
	`cdnthumbwidth="249" cdnthumbheight="180" /></msg>`

// wxImgMsgXML 新版微信（4.x）实测报文：标签是 <imgmsg>，带换行缩进
const wxImgMsgXML = "wxid_abc:\n<?xml version=\"1.0\"?>\n<msg>\n\t<imgmsg aeskey=\"52b62f956fb8dc968be5fb02e0781f73\" encryver=\"0\" " +
	"cdnthumbaeskey=\"52b62f956fb8dc968be5fb02e0781f73\" cdnthumburl=\"305f020100044b3049\" " +
	"cdnmidimgurl=\"305f020100044bmid\" length=\"152340\" md5=\"md5new4x\" " +
	"cdnthumbwidth=\"720\" cdnthumbheight=\"960\" /></msg>"

func TestParseRawImageMsgTag(t *testing.T) {
	meta, ok := parseRawImage(mkImageMsg(wxImgMsgXML))
	if !ok {
		t.Fatal("新版 <imgmsg> 报文应解析成功")
	}
	if meta.Md5 != "md5new4x" || meta.Key != "52b62f956fb8dc968be5fb02e0781f73" {
		t.Fatalf("md5/aeskey 不对: %q %q", meta.Md5, meta.Key)
	}
	if meta.Size != 152340 || meta.Width != 720 || meta.Height != 960 {
		t.Fatalf("size/dim 不对: %d %dx%d", meta.Size, meta.Width, meta.Height)
	}
}

func TestParseRawImageStandard(t *testing.T) {
	meta, ok := parseRawImage(mkImageMsg(wxImgXML))
	if !ok {
		t.Fatal("标准报文应解析成功")
	}
	if meta.Md5 != "md5abc123" {
		t.Fatalf("md5 不对: %q", meta.Md5)
	}
	if meta.Key != "aesk_123" {
		t.Fatalf("aeskey 不对: %q", meta.Key)
	}
	if meta.Size != 88042 {
		t.Fatalf("size 不对: %d", meta.Size)
	}
	if meta.Width != 249 || meta.Height != 180 {
		t.Fatalf("宽高不对: %dx%d", meta.Width, meta.Height)
	}
	if !strings.HasPrefix(meta.Url, "3057020100044bmid") {
		t.Fatalf("url 应优先取中图: %q", meta.Url)
	}
}

func TestParseRawImageAttrVariants(t *testing.T) {
	// 属性名变体（不同客户端/版本）：无 md5 但有长度与宽高别名
	xmlStr := `<msg><img aeskey="k2" datalen="2048" width="800" height="600" /></msg>`
	meta, ok := parseRawImage(mkImageMsg(xmlStr))
	if !ok {
		t.Fatal("变体报文应解析成功")
	}
	if meta.Size != 2048 || meta.Width != 800 || meta.Height != 600 {
		t.Fatalf("变体字段解析不对: size=%d dim=%dx%d", meta.Size, meta.Width, meta.Height)
	}
}

func TestParseRawImageMissing(t *testing.T) {
	if _, ok := parseRawImage(&message.Message{Type: message.TypeImage}); ok {
		t.Fatal("无 Raw 应失败")
	}
	if _, ok := parseRawImage(mkImageMsg(`<msg><emoji fromusername="x"/></msg>`)); ok {
		t.Fatal("无 img 标签应失败")
	}
	// 全零属性（真啥都没有）也算失败，不进环
	if _, ok := parseRawImage(mkImageMsg(`<msg><img aeskey="" length="0" md5="" /></msg>`)); ok {
		t.Fatal("全零属性应视为失败")
	}
}

func TestImageContextLabel(t *testing.T) {
	got := imageContextLabel(mkImageMsg(wxImgXML))
	if got != "[图片 249x180 85KB]" {
		t.Fatalf("上下文标签不对: %q", got)
	}
	// 抢救失败也不许出现 0B
	if got := imageContextLabel(&message.Message{Type: message.TypeImage, Content: "[0 x 0] 0B"}); strings.Contains(got, "0B") || got == "" {
		t.Fatalf("兜底标签不应含 0B: %q", got)
	}
}

func TestImageMediaOrRawPrefersMedia(t *testing.T) {
	// 消息自带完整 Media（宿主修好后）应优先，不碰 Raw
	msg := mkImageMsg(wxImgXML)
	msg.Data = &message.Message_Image{Image: &message.ImageData{
		Media: &message.Media{Md5: "native_md5", Key: "native_key", Size: 777},
	}}
	md5v, key, _, size, ok := imageMediaOrRaw(msg)
	if !ok || md5v != "native_md5" || key != "native_key" || size != 777 {
		t.Fatalf("自带 Media 应优先: %v %v %d %v", md5v, key, size, ok)
	}
}
