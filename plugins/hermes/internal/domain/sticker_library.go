package domain

import (
	"errors"
	"strings"
	"time"
)

type StickerAsset struct {
	ID        string
	MIMEType  string
	Path      string
	Size      int64
	SHA256    string
	CreatedAt time.Time
}

func (s StickerAsset) Validate() error {
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.Path) == "" {
		return errors.New("sticker asset requires id and path")
	}
	if strings.TrimSpace(s.MIMEType) == "" || strings.TrimSpace(s.SHA256) == "" {
		return errors.New("sticker asset requires mime_type and sha256")
	}
	if s.Size <= 0 || s.CreatedAt.IsZero() {
		return errors.New("sticker asset size and created_at are invalid")
	}
	return nil
}

type StickerLabel struct {
	StickerID         string
	Description       string
	DescriptionNorm   string
	SourceSessionID   string
	SourceEventID     string
	SourceMessageID   string
	SourceSpeakerID   string
	SourceSpeakerName string
	CollectedByID     string
	CollectedByName   string
	CreatedAt         time.Time
}

func (s StickerLabel) Validate() error {
	if strings.TrimSpace(s.StickerID) == "" || strings.TrimSpace(s.Description) == "" ||
		strings.TrimSpace(s.DescriptionNorm) == "" || s.CreatedAt.IsZero() {
		return errors.New("sticker label is invalid")
	}
	return nil
}

type StickerSearchTerm struct {
	Value  string
	Weight int
}

type StickerLibraryMatch struct {
	StickerID   string
	Description string
	Score       int
}

type StickerCollectionResult struct {
	Asset        StickerAsset
	AssetCreated bool
	LabelCreated bool
}
