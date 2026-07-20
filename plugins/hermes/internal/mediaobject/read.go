package mediaobject

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

func (s *Store) Read(
	ctx context.Context,
	objectID string,
	expectedKind string,
) ([]byte, domain.MediaObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, domain.MediaObject{}, err
	}
	object, err := s.repository.GetMediaObject(ctx, objectID)
	if err != nil {
		return nil, domain.MediaObject{}, err
	}
	if object.Kind != expectedKind {
		return nil, domain.MediaObject{}, fmt.Errorf("media object %s has kind %s", object.ID, object.Kind)
	}
	if err := verifyStoredFile(s.config.Directory, object); err != nil {
		return nil, domain.MediaObject{}, err
	}
	file, err := os.Open(object.Path)
	if err != nil {
		return nil, domain.MediaObject{}, fmt.Errorf("open media object: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, object.Size+1))
	if err != nil || int64(len(data)) != object.Size {
		return nil, domain.MediaObject{}, errors.New("media object size changed on disk")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != object.SHA256 {
		return nil, domain.MediaObject{}, errors.New("media object digest changed on disk")
	}
	return data, object, nil
}

func verifyStoredFile(root string, object domain.MediaObject) error {
	relative, err := filepath.Rel(root, object.Path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("media object path escapes configured directory")
	}
	info, err := os.Stat(object.Path)
	if err != nil {
		return fmt.Errorf("stat media object: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != object.Size {
		return errors.New("media object file metadata is invalid")
	}
	return nil
}
