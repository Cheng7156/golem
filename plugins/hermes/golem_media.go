package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/message"
	"google.golang.org/protobuf/proto"
)

const maxInboundMediaBytes = 16 << 20

type golemMediaResolver struct {
	message message.Ability
}

func (r golemMediaResolver) Resolve(
	ctx context.Context,
	media []domain.InboundMedia,
) ([]domain.InboundMedia, error) {
	resolved := append([]domain.InboundMedia(nil), media...)
	for index := range resolved {
		item := &resolved[index]
		if len(item.Data) > 0 || len(item.DownloadSource) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.message == nil {
			return nil, errors.New("message ability is unavailable")
		}
		var source message.Message
		if err := proto.Unmarshal(item.DownloadSource, &source); err != nil {
			return nil, fmt.Errorf("decode persisted image source: %w", err)
		}
		reader, err := r.message.Download(&source)
		if err != nil {
			if imageDownloadUnsupported(err) {
				// The lib-mode host cannot materialize inbound images. Keep the
				// structured [image] turn so Hermes can still observe or reply
				// based on verified identity and conversation context.
				item.DownloadSource = nil
				continue
			}
			return nil, fmt.Errorf("download WeChat image: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxInboundMediaBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read WeChat image: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close WeChat image: %w", closeErr)
		}
		if len(data) == 0 {
			return nil, errors.New("downloaded WeChat image is empty")
		}
		if len(data) > maxInboundMediaBytes {
			return nil, errors.New("downloaded WeChat image exceeds 16 MiB")
		}
		mimeType := http.DetectContentType(data)
		if !strings.HasPrefix(mimeType, "image/") || mimeType == "image/svg+xml" {
			return nil, fmt.Errorf("downloaded WeChat media is not a supported image: %s", mimeType)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item.Data = data
		item.MIMEType = mimeType
		item.DownloadSource = nil
	}
	return resolved, nil
}

func imageDownloadUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "DownloadImg not supported in lib mode")
}
