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
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const (
	defaultCollectBatch = 32
	objectIDPrefix      = "media_"
)

type Store struct {
	config     Config
	repository Repository
	mu         sync.Mutex
	now        func() time.Time
}

func NewStore(config Config, repository Repository) (*Store, error) {
	config.Directory = filepath.Clean(strings.TrimSpace(config.Directory))
	if config.Directory == "." || config.MaxStorageBytes <= 0 || config.Retention <= 0 {
		return nil, errors.New("media object store configuration is invalid")
	}
	if repository == nil {
		return nil, errors.New("media object repository is required")
	}
	if config.CollectBatch <= 0 {
		config.CollectBatch = defaultCollectBatch
	}
	if err := os.MkdirAll(config.Directory, 0o750); err != nil {
		return nil, fmt.Errorf("create media object directory: %w", err)
	}
	return &Store{config: config, repository: repository, now: time.Now}, nil
}

func (s *Store) Save(ctx context.Context, request SaveRequest) (domain.MediaObject, error) {
	if err := validateSaveRequest(request); err != nil {
		return domain.MediaObject{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	temporary, object, err := s.writeTemporary(request)
	if err != nil {
		return domain.MediaObject{}, err
	}
	defer os.Remove(temporary)
	stored, err := s.findExisting(ctx, object)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, storeport.ErrNotFound) {
		return domain.MediaObject{}, err
	}
	if err := s.ensureCapacity(ctx, object.Size); err != nil {
		return domain.MediaObject{}, err
	}
	return s.install(ctx, temporary, object)
}

func validateSaveRequest(request SaveRequest) error {
	if request.Reader == nil || request.MaxBytes <= 0 {
		return errors.New("media save requires a reader and positive max_bytes")
	}
	if request.Kind != domain.MediaKindVideo && request.Kind != domain.MediaKindThumbnail {
		return errors.New("media save kind is invalid")
	}
	if strings.TrimSpace(request.MIMEType) == "" {
		return errors.New("media save MIME type is empty")
	}
	return nil
}

func (s *Store) writeTemporary(request SaveRequest) (string, domain.MediaObject, error) {
	file, err := os.CreateTemp(s.config.Directory, ".incoming-*")
	if err != nil {
		return "", domain.MediaObject{}, fmt.Errorf("create media temporary file: %w", err)
	}
	path := file.Name()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(request.Reader, request.MaxBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written == 0 || written > request.MaxBytes {
		_ = os.Remove(path)
		return "", domain.MediaObject{}, mediaWriteError(mediaWriteFailure{
			copyErr: copyErr, closeErr: closeErr, written: written, maximum: request.MaxBytes,
		})
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	now := s.now()
	return path, domain.MediaObject{
		ID: objectIDPrefix + digest, Kind: request.Kind, MIMEType: strings.TrimSpace(request.MIMEType),
		Size: written, SHA256: digest, RetainedUntil: now.Add(s.config.Retention),
		CreatedAt: now, LastAccessAt: now,
	}, nil
}

type mediaWriteFailure struct {
	copyErr  error
	closeErr error
	written  int64
	maximum  int64
}

func mediaWriteError(failure mediaWriteFailure) error {
	if failure.copyErr != nil {
		return fmt.Errorf("write media object: %w", failure.copyErr)
	}
	if failure.closeErr != nil {
		return fmt.Errorf("close media object: %w", failure.closeErr)
	}
	if failure.written == 0 {
		return errors.New("media object is empty")
	}
	return fmt.Errorf("media object exceeds %d bytes", failure.maximum)
}
