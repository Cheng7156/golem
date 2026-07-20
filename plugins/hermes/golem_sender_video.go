package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

type outboundVideoMedia struct {
	video         []byte
	thumbnail     []byte
	videoMIME     string
	thumbnailMIME string
}

func (s *golemSender) videoMessage(
	ctx context.Context,
	receiver *contact.Contact,
	payloadJSON json.RawMessage,
) (*message.Message, error) {
	if s.mediaObjects == nil {
		return nil, errors.New("video media object reader is unavailable")
	}
	var payload domain.VideoOutput
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("decode video outbox: %w", err)
	}
	media, err := s.readVideoMedia(ctx, payload)
	if err != nil {
		return nil, err
	}
	if err := validateOutboundVideo(media); err != nil {
		return nil, err
	}
	digest := md5.Sum(media.video)
	md5Text := hex.EncodeToString(digest[:])
	return &message.Message{
		Type: message.TypeVideo, Receiver: receiver, Content: strings.TrimSpace(payload.Title),
		Data: &message.Message_Video{Video: &message.VideoData{
			Media:    &message.Media{Data: media.video, Size: uint32(len(media.video)), Md5: md5Text},
			Duration: payload.Duration, Thumb: media.thumbnail, NewMd5: md5Text,
		}},
	}, nil
}

func (s *golemSender) readVideoMedia(
	ctx context.Context,
	payload domain.VideoOutput,
) (outboundVideoMedia, error) {
	videoData, videoObject, err := s.mediaObjects.Read(ctx, payload.ObjectID, domain.MediaKindVideo)
	if err != nil {
		return outboundVideoMedia{}, fmt.Errorf("read video object: %w", err)
	}
	thumbData, thumbObject, err := s.mediaObjects.Read(
		ctx, payload.ThumbObjectID, domain.MediaKindThumbnail,
	)
	if err != nil {
		return outboundVideoMedia{}, fmt.Errorf("read video thumbnail: %w", err)
	}
	return outboundVideoMedia{
		video: videoData, thumbnail: thumbData,
		videoMIME: videoObject.MIMEType, thumbnailMIME: thumbObject.MIMEType,
	}, nil
}

func validateOutboundVideo(media outboundVideoMedia) error {
	if media.videoMIME != "video/mp4" || media.thumbnailMIME != "image/jpeg" {
		return errors.New("video media objects have invalid MIME types")
	}
	if len(media.video) < 12 || !bytes.Equal(media.video[4:8], []byte("ftyp")) {
		return errors.New("video media object is not an MP4 file")
	}
	if err := validateImage(media.thumbnail); err != nil {
		return fmt.Errorf("invalid video thumbnail: %w", err)
	}
	if len(media.video)+len(media.thumbnail) > maxOutboundVideoEnvelopeBytes {
		return errors.New("video message exceeds the plugin gRPC envelope")
	}
	return nil
}
