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
	storeport "golem_plugin_hermes/internal/store"
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

func TestLocalLibraryInventoryIsGlobalAcrossSourceSessions(t *testing.T) {
	library, _ := openStickerLibrary(t, 2048, "owner")
	group := libraryCollectRequest("群聊收藏", libraryTestPNG, true)
	group.SourceSessionID = "chatroom:room-a"
	if _, err := library.Collect(context.Background(), group); err != nil {
		t.Fatalf("Collect group sticker: %v", err)
	}
	privateData := append([]byte(nil), libraryTestPNG...)
	privateData[len(privateData)-1]++
	private := libraryCollectRequest("私聊收藏", privateData, true)
	private.SourceSessionID = "private:wxid-owner"
	if _, err := library.Collect(context.Background(), private); err != nil {
		t.Fatalf("Collect private sticker: %v", err)
	}

	inventory, err := library.Inventory(context.Background(), 20, 0)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if inventory.Total != 2 || len(inventory.Items) != 2 {
		t.Fatalf("global inventory=%#v", inventory)
	}
	descriptions := inventory.Items[0].Description + "；" + inventory.Items[1].Description
	if !strings.Contains(descriptions, "群聊收藏") || !strings.Contains(descriptions, "私聊收藏") {
		t.Fatalf("global inventory descriptions=%q", descriptions)
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

type recentLibraryRepository struct {
	*sqlite.Store
	recent    domain.StickerAsset
	recentErr error
	deleteErr error
}

func (r *recentLibraryRepository) FindRecentSentStickerAsset(
	context.Context,
	string,
) (domain.StickerAsset, error) {
	if r.recentErr != nil {
		return domain.StickerAsset{}, r.recentErr
	}
	return r.recent, nil
}

func (r *recentLibraryRepository) DeleteStickerAsset(ctx context.Context, id string) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.Store.DeleteStickerAsset(ctx, id)
}

func TestLocalLibraryManagesRecentStickerWithoutSearch(t *testing.T) {
	ctx := context.Background()
	library, repository := openManagedStickerLibrary(t, "any")
	collected, err := library.Collect(ctx, libraryCollectRequest("欢天喜地", libraryTestPNG, true))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	repository.recent, err = repository.GetStickerAsset(ctx, collected.StickerID)
	if err != nil {
		t.Fatalf("GetStickerAsset: %v", err)
	}
	assetPath := repository.recent.Path

	if _, err := library.ManageRecent(ctx, LibraryManageRequest{
		Action: "update_description", Description: "冷眼闭嘴",
		SessionID: "private:owner", Principal: domain.Principal{ID: "owner", IsOwner: false},
	}); !errors.Is(err, ErrManagementForbidden) {
		t.Fatalf("non-owner management error=%v", err)
	}
	updated, err := library.ManageRecent(ctx, LibraryManageRequest{
		Action: "update_description", Description: "冷眼闭嘴",
		SessionID: "private:owner", Principal: domain.Principal{ID: "owner", Name: "主人", IsOwner: true},
	})
	if err != nil || updated.Description != "冷眼闭嘴" {
		t.Fatalf("ManageRecent update=%#v err=%v", updated, err)
	}
	oldMatches, err := library.Search(ctx, ProviderSearchRequest{Query: "欢天喜地", Limit: 5, Page: 1})
	if err != nil || len(oldMatches) != 0 {
		t.Fatalf("old Search=%#v err=%v", oldMatches, err)
	}
	newMatches, err := library.Search(ctx, ProviderSearchRequest{Query: "闭嘴", Limit: 5, Page: 1})
	if err != nil || len(newMatches) != 1 || newMatches[0].Description != "冷眼闭嘴" {
		t.Fatalf("new Search=%#v err=%v", newMatches, err)
	}

	repository.deleteErr = errors.New("database unavailable")
	if _, err := library.ManageRecent(ctx, LibraryManageRequest{
		Action: "delete", SessionID: "private:owner",
		Principal: domain.Principal{ID: "owner", IsOwner: true},
	}); err == nil {
		t.Fatal("delete unexpectedly succeeded")
	}
	if _, err := os.Stat(assetPath); err != nil {
		t.Fatalf("failed delete did not restore file: %v", err)
	}
	repository.deleteErr = nil
	deleted, err := library.ManageRecent(ctx, LibraryManageRequest{
		Action: "delete", SessionID: "private:owner",
		Principal: domain.Principal{ID: "owner", IsOwner: true},
	})
	if err != nil || deleted.Action != "delete" {
		t.Fatalf("ManageRecent delete=%#v err=%v", deleted, err)
	}
	if _, err := os.Stat(assetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file error=%v", err)
	}
	if _, err := repository.GetStickerAsset(ctx, collected.StickerID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("deleted repository asset error=%v", err)
	}
	repository.recentErr = storeport.ErrNotFound
	if _, err := library.ManageRecent(ctx, LibraryManageRequest{
		Action: "delete", SessionID: "private:owner",
		Principal: domain.Principal{ID: "owner", IsOwner: true},
	}); !errors.Is(err, ErrRecentStickerNotFound) {
		t.Fatalf("repeated delete error=%v", err)
	}
}

func openManagedStickerLibrary(
	t *testing.T,
	policy string,
) (*LocalLibrary, *recentLibraryRepository) {
	t.Helper()
	repository, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "hermes.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	managed := &recentLibraryRepository{Store: repository}
	library, err := NewLocalLibrary(LibraryConfig{
		Directory: filepath.Join(t.TempDir(), "stickers"), MaxStorageBytes: 1024,
		MaxMediaBytes: int64(len(libraryTestPNG)), CollectionPolicy: policy,
	}, managed)
	if err != nil {
		t.Fatalf("NewLocalLibrary: %v", err)
	}
	return library, managed
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
