package mediaobject

import (
	"context"
	"errors"
	"io"
	"time"

	"golem_plugin_hermes/internal/domain"
)

var ErrStorageFull = errors.New("media object storage budget is exhausted")

type Repository interface {
	CreateMediaObject(context.Context, domain.MediaObject) (domain.MediaObject, bool, error)
	GetMediaObject(context.Context, string) (domain.MediaObject, error)
	RetainMediaObject(context.Context, string, time.Time) error
	MediaStorageBytes(context.Context) (int64, error)
	ListCollectibleMediaObjects(context.Context, time.Time, int) ([]domain.MediaObject, error)
	DeleteMediaObject(context.Context, string) error
}

type Config struct {
	Directory       string
	MaxStorageBytes int64
	Retention       time.Duration
	CollectBatch    int
}

type SaveRequest struct {
	Kind     string
	MIMEType string
	Reader   io.Reader
	MaxBytes int64
}

type Reader interface {
	Read(context.Context, string, string) ([]byte, domain.MediaObject, error)
}
