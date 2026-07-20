package mediaobject

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
)

func TestStoreSavesDeduplicatesAndReadsObject(t *testing.T) {
	store, repository := openMediaStore(t, 1024)
	request := SaveRequest{
		Kind: domain.MediaKindVideo, MIMEType: "video/mp4",
		Reader: bytes.NewReader([]byte("video-data")), MaxBytes: 100,
	}
	first, err := store.Save(context.Background(), request)
	if err != nil {
		t.Fatalf("Save first: %v", err)
	}
	request.Reader = bytes.NewReader([]byte("video-data"))
	second, err := store.Save(context.Background(), request)
	if err != nil || second.ID != first.ID {
		t.Fatalf("Save duplicate: second=%#v error=%v", second, err)
	}
	total, _ := repository.MediaStorageBytes(context.Background())
	data, _, readErr := store.Read(context.Background(), first.ID, domain.MediaKindVideo)
	if readErr != nil || string(data) != "video-data" || total != first.Size {
		t.Fatalf("data=%q total=%d error=%v", data, total, readErr)
	}
}

func TestStoreCollectsExpiredObjectForCapacity(t *testing.T) {
	store, repository := openMediaStore(t, 8)
	current := time.Now()
	store.now = func() time.Time { return current }
	first := saveBytes(t, store, saveTestRequest{
		value: "first", kind: domain.MediaKindVideo, mimeType: "video/mp4",
	})
	current = current.Add(2 * time.Hour)
	second := saveBytes(t, store, saveTestRequest{
		value: "second", kind: domain.MediaKindVideo, mimeType: "video/mp4",
	})
	if first.ID == second.ID {
		t.Fatal("different payloads produced the same object")
	}
	if _, err := repository.GetMediaObject(context.Background(), first.ID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("expired object remains in repository: %v", err)
	}
	if _, err := os.Stat(first.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired object remains on disk: %v", err)
	}
}

func TestReadRejectsCorruptedObject(t *testing.T) {
	store, _ := openMediaStore(t, 1024)
	object := saveBytes(t, store, saveTestRequest{
		value: "thumbnail", kind: domain.MediaKindThumbnail, mimeType: "image/jpeg",
	})
	if err := os.WriteFile(object.Path, []byte("corrupt!!"), 0o600); err != nil {
		t.Fatalf("corrupt test object: %v", err)
	}
	if _, _, err := store.Read(context.Background(), object.ID, domain.MediaKindThumbnail); err == nil {
		t.Fatal("Read accepted corrupted media object")
	}
}

func openMediaStore(t *testing.T, budget int64) (*Store, *sqlite.Store) {
	t.Helper()
	repository, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	store, err := NewStore(Config{
		Directory: filepath.Join(t.TempDir(), "media"), MaxStorageBytes: budget,
		Retention: time.Hour,
	}, repository)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, repository
}

type saveTestRequest struct {
	value    string
	kind     string
	mimeType string
}

func saveBytes(t *testing.T, store *Store, request saveTestRequest) domain.MediaObject {
	t.Helper()
	object, err := store.Save(context.Background(), SaveRequest{
		Kind: request.kind, MIMEType: request.mimeType,
		Reader: bytes.NewReader([]byte(request.value)), MaxBytes: 100,
	})
	if err != nil {
		t.Fatalf("Save %q: %v", request.value, err)
	}
	return object
}
