package domain

import (
	"errors"
	"strings"
	"time"
)

const (
	MediaKindVideo     = "video"
	MediaKindThumbnail = "thumbnail"
)

type MediaObject struct {
	ID            string
	Kind          string
	MIMEType      string
	Path          string
	Size          int64
	SHA256        string
	RetainedUntil time.Time
	CreatedAt     time.Time
	LastAccessAt  time.Time
}

func (m MediaObject) Validate() error {
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Path) == "" {
		return errors.New("media object requires id and path")
	}
	if m.Kind != MediaKindVideo && m.Kind != MediaKindThumbnail {
		return errors.New("media object kind is invalid")
	}
	if strings.TrimSpace(m.MIMEType) == "" || strings.TrimSpace(m.SHA256) == "" {
		return errors.New("media object requires mime_type and sha256")
	}
	if m.Size <= 0 || m.CreatedAt.IsZero() || m.RetainedUntil.IsZero() {
		return errors.New("media object size and timestamps are invalid")
	}
	return nil
}
