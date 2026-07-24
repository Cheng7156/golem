package sticker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/store/sqlite"
)

var libraryTestPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 24)...)

func TestLocalLibraryCollectsDeduplicatesSearchesAndMaterializes(t *testing.T) {
	library, repository := openStickerLibrary(t, 1024, "owner")
	ctx := context.Background()
	first, err := library.Collect(ctx, libraryCollectRequest("大傻逼", libraryTestPNG, true))
	if err != nil {
		t.Fatalf("Collect first: %v", err)
	}
	if !first.AssetCreated || !first.LabelCreated || first.StickerID == "" {
		t.Fatalf("first result=%#v", first)
	}
	duplicate, err := library.Collect(ctx, libraryCollectRequest("大傻逼", libraryTestPNG, true))
	if err != nil || duplicate.AssetCreated || duplicate.LabelCreated {
		t.Fatalf("duplicate result=%#v err=%v", duplicate, err)
	}
	alias, err := library.Collect(ctx, libraryCollectRequest("震惊又嫌弃 😂", libraryTestPNG, true))
	if err != nil || alias.AssetCreated || !alias.LabelCreated {
		t.Fatalf("alias result=%#v err=%v", alias, err)
	}
	total, err := repository.StickerLibraryStorageBytes(ctx)
	if err != nil || total != int64(len(libraryTestPNG)) {
		t.Fatalf("storage=%d err=%v", total, err)
	}

	for _, query := range []string{"傻逼", "大傻比", "震惊", "😂"} {
		matches, searchErr := library.Search(ctx, ProviderSearchRequest{Query: query, Limit: 5, Page: 1})
		if searchErr != nil || len(matches) != 1 || matches[0].Reference != first.StickerID {
			t.Fatalf("Search(%q)=%#v err=%v", query, matches, searchErr)
		}
		if !strings.Contains(matches[0].Description, "大傻逼") ||
			!strings.Contains(matches[0].Description, "震惊又嫌弃") {
			t.Fatalf("Search(%q) description=%q", query, matches[0].Description)
		}
	}

	matches, err := library.Search(ctx, ProviderSearchRequest{Query: "完全无关", Limit: 5, Page: 1})
	if err != nil || len(matches) != 0 {
		t.Fatalf("unrelated Search=%#v err=%v", matches, err)
	}
	selected, err := library.Search(ctx, ProviderSearchRequest{Query: "大傻逼", Limit: 1, Page: 1})
	if err != nil || len(selected) != 1 {
		t.Fatalf("exact Search=%#v err=%v", selected, err)
	}
	output, err := library.Materialize(ctx, selected[0])
	if err != nil || !bytes.Equal(output.Data, libraryTestPNG) || output.MIMEType != "image/png" {
		t.Fatalf("Materialize=%#v err=%v", output, err)
	}
	asset, err := repository.GetStickerAsset(ctx, first.StickerID)
	if err != nil || asset.ID != first.StickerID {
		t.Fatalf("stored asset=%#v err=%v", asset, err)
	}
}

func TestLocalLibraryEnforcesCollectionPolicyAndStorageBudget(t *testing.T) {
	library, _ := openStickerLibrary(t, int64(len(libraryTestPNG)), "owner")
	ctx := context.Background()
	if _, err := library.Collect(ctx, libraryCollectRequest("不允许", libraryTestPNG, false)); !errors.Is(err, ErrCollectionForbidden) {
		t.Fatalf("non-owner collection error=%v", err)
	}
	if _, err := library.Collect(ctx, libraryCollectRequest("第一张", libraryTestPNG, true)); err != nil {
		t.Fatalf("Collect within budget: %v", err)
	}
	other := append([]byte(nil), libraryTestPNG...)
	other[len(other)-1]++
	if _, err := library.Collect(ctx, libraryCollectRequest("第二张", other, true)); !errors.Is(err, ErrLibraryStorageFull) {
		t.Fatalf("over-budget collection error=%v", err)
	}
}

func TestLocalLibraryAnyPolicyAllowsNonOwnerCollection(t *testing.T) {
	library, _ := openStickerLibrary(t, 1024, "any")
	result, err := library.Collect(
		context.Background(), libraryCollectRequest("群友收藏", libraryTestPNG, false),
	)
	if err != nil || !result.AssetCreated {
		t.Fatalf("Collect with any policy=%#v err=%v", result, err)
	}
}

func TestLocalLibraryConcurrentDuplicateCollectionIsIdempotent(t *testing.T) {
	library, repository := openStickerLibrary(t, 1024, "owner")
	ctx := context.Background()
	const workers = 12
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := library.Collect(ctx, libraryCollectRequest("并发收藏", libraryTestPNG, true))
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent Collect: %v", err)
		}
	}
	total, err := repository.StickerLibraryStorageBytes(ctx)
	if err != nil || total != int64(len(libraryTestPNG)) {
		t.Fatalf("storage=%d err=%v", total, err)
	}
}

func TestLocalLibraryRejectsSymlinkedAsset(t *testing.T) {
	library, repository := openStickerLibrary(t, 1024, "owner")
	ctx := context.Background()
	collected, err := library.Collect(ctx, libraryCollectRequest("测试", libraryTestPNG, true))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	asset, err := repository.GetStickerAsset(ctx, collected.StickerID)
	if err != nil {
		t.Fatalf("GetStickerAsset: %v", err)
	}
	target := filepath.Join(t.TempDir(), "same.png")
	if err := os.WriteFile(target, libraryTestPNG, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Remove(asset.Path); err != nil {
		t.Fatalf("Remove asset: %v", err)
	}
	if err := os.Symlink(target, asset.Path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	_, err = library.Materialize(ctx, ProviderCandidate{
		Reference: collected.StickerID, Description: "测试",
	})
	if err == nil || !strings.Contains(err.Error(), "metadata is invalid") {
		t.Fatalf("Materialize symlink error=%v", err)
	}
}

func openStickerLibrary(
	t *testing.T,
	budget int64,
	policy string,
) (*LocalLibrary, *sqlite.Store) {
	t.Helper()
	repository, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	library, err := NewLocalLibrary(LibraryConfig{
		Directory: filepath.Join(t.TempDir(), "stickers"), MaxStorageBytes: budget,
		MaxMediaBytes: int64(len(libraryTestPNG)), CollectionPolicy: policy,
	}, repository)
	if err != nil {
		t.Fatalf("NewLocalLibrary: %v", err)
	}
	return library, repository
}

func libraryCollectRequest(
	description string,
	data []byte,
	owner bool,
) LibraryCollectRequest {
	return LibraryCollectRequest{
		Description: description, Data: append([]byte(nil), data...), MIMEType: "image/png",
		SourceSessionID: "chatroom:room", SourceEventID: "event-1",
		SourceMessageID: "message-1", SourceSpeakerID: "wxid-sender",
		SourceSpeakerName: "发送者", Collector: domain.Principal{
			ID: "wxid-collector", Name: "收藏者", IsOwner: owner,
		},
	}
}
