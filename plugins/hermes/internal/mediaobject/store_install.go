package mediaobject

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func (s *Store) findExisting(
	ctx context.Context,
	object domain.MediaObject,
) (domain.MediaObject, error) {
	stored, err := s.repository.GetMediaObject(ctx, object.ID)
	if err != nil {
		return domain.MediaObject{}, err
	}
	if stored.Size != object.Size || stored.SHA256 != object.SHA256 {
		return domain.MediaObject{}, errors.New("media object digest collision")
	}
	if err := verifyStoredFile(s.config.Directory, stored); err != nil {
		return domain.MediaObject{}, err
	}
	if err := s.repository.RetainMediaObject(ctx, stored.ID, object.RetainedUntil); err != nil {
		return domain.MediaObject{}, err
	}
	stored.RetainedUntil = object.RetainedUntil
	return stored, nil
}

func (s *Store) install(
	ctx context.Context,
	temporary string,
	object domain.MediaObject,
) (domain.MediaObject, error) {
	extension, err := mediaExtension(object.MIMEType)
	if err != nil {
		return domain.MediaObject{}, err
	}
	directory := filepath.Join(s.config.Directory, object.SHA256[:2])
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return domain.MediaObject{}, fmt.Errorf("create media shard directory: %w", err)
	}
	object.Path = filepath.Join(directory, object.SHA256+extension)
	if _, err := os.Stat(object.Path); err == nil {
		return domain.MediaObject{}, errors.New("untracked media object file already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.MediaObject{}, err
	}
	if err := os.Rename(temporary, object.Path); err != nil {
		return domain.MediaObject{}, fmt.Errorf("install media object: %w", err)
	}
	stored, created, err := s.repository.CreateMediaObject(ctx, object)
	if err != nil || !created {
		_ = os.Remove(object.Path)
		if err == nil {
			err = errors.New("media object repository rejected a new object")
		}
		return domain.MediaObject{}, err
	}
	return stored, nil
}

func (s *Store) ensureCapacity(ctx context.Context, incoming int64) error {
	total, err := s.repository.MediaStorageBytes(ctx)
	if err != nil {
		return err
	}
	for total+incoming > s.config.MaxStorageBytes {
		freed, collectErr := s.collectOneBatch(ctx)
		if collectErr != nil {
			return collectErr
		}
		if freed == 0 {
			return ErrStorageFull
		}
		total -= freed
	}
	return nil
}

func (s *Store) collectOneBatch(ctx context.Context) (int64, error) {
	objects, err := s.repository.ListCollectibleMediaObjects(ctx, s.now(), s.config.CollectBatch)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, object := range objects {
		if err := s.repository.DeleteMediaObject(ctx, object.ID); err != nil {
			if errors.Is(err, storeport.ErrConflict) {
				continue
			}
			return freed, err
		}
		if err := os.Remove(object.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return freed, fmt.Errorf("remove collected media object: %w", err)
		}
		freed += object.Size
	}
	return freed, nil
}

func mediaExtension(mimeType string) (string, error) {
	switch mimeType {
	case "video/mp4":
		return ".mp4", nil
	case "image/jpeg":
		return ".jpg", nil
	default:
		return "", fmt.Errorf("unsupported media object MIME type %q", mimeType)
	}
}
