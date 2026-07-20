package video

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"strings"
)

const containerProbeBytes = 512

func validateDownloadedMedia(path, rawContentType string) error {
	contentType, _, _ := mime.ParseMediaType(rawContentType)
	if rejectedVideoContentType(contentType) {
		return fmt.Errorf("video source returned unsupported content type %s", contentType)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, containerProbeBytes)
	count, err := file.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !supportedVideoMagic(buffer[:count]) {
		return errors.New("video source is not a supported binary media container")
	}
	return nil
}

func rejectedVideoContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "text/") || value == "application/json" ||
		value == "application/vnd.apple.mpegurl" || value == "application/x-mpegurl"
}

func supportedVideoMagic(value []byte) bool {
	if len(value) >= 12 && bytes.Equal(value[4:8], []byte("ftyp")) {
		return true
	}
	if hasPrefix(value, []byte{0x1a, 0x45, 0xdf, 0xa3}) || hasPrefix(value, []byte("FLV")) ||
		hasPrefix(value, []byte("OggS")) || hasPrefix(value, []byte{0x30, 0x26, 0xb2, 0x75}) {
		return true
	}
	if len(value) >= 12 && bytes.Equal(value[:4], []byte("RIFF")) && bytes.Equal(value[8:12], []byte("AVI ")) {
		return true
	}
	if hasPrefix(value, []byte{0x00, 0x00, 0x01, 0xba}) || hasPrefix(value, []byte{0x00, 0x00, 0x01, 0xb3}) {
		return true
	}
	return len(value) > 188 && value[0] == 0x47 && value[188] == 0x47
}

func hasPrefix(value, prefix []byte) bool {
	return len(value) >= len(prefix) && bytes.Equal(value[:len(prefix)], prefix)
}
