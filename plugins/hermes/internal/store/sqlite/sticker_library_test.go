package sqlite_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
	"golem_plugin_hermes/internal/store/sqlite"
)

func TestStickerLibraryFindsOnlyLatestSuccessfullySentLocalSticker(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	localData := []byte("local-sticker-bytes")
	asset := storeTestSticker(t, value, localData, "旧描述")
	fixture := createRunningRun(t, value, "recent-sticker")
	localPayload, _ := json.Marshal(domain.EmojiOutput{Data: localData, MIMEType: "image/png"})
	externalPayload, _ := json.Marshal(domain.EmojiOutput{Data: []byte("external-sticker")})
	items, err := value.CommitRunSuccess(ctx, fixture.run.ID, fixture.run.LeaseToken, []domain.OutboxDraft{
		{SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID, Kind: "emoji", Payload: localPayload},
		{SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID, Kind: "emoji", Payload: externalPayload},
	})
	if err != nil || len(items) != 2 {
		t.Fatalf("CommitRunSuccess items=%d err=%v", len(items), err)
	}
	first, err := value.LeaseNextOutbox(ctx, time.Now(), time.Minute)
	if err != nil || first.ID != items[0].ID {
		t.Fatalf("Lease local outbox=%#v err=%v", first, err)
	}
	if err := value.MarkOutboxSent(ctx, first.ID, first.LeaseToken, 1001, time.Now()); err != nil {
		t.Fatalf("Mark local sent: %v", err)
	}
	found, err := value.FindRecentSentStickerAsset(ctx, fixture.event.SessionID)
	if err != nil || found.ID != asset.ID {
		t.Fatalf("Find local asset=%#v err=%v", found, err)
	}
	second, err := value.LeaseNextOutbox(ctx, time.Now(), time.Minute)
	if err != nil || second.ID != items[1].ID {
		t.Fatalf("Lease external outbox=%#v err=%v", second, err)
	}
	if err := value.MarkOutboxSent(ctx, second.ID, second.LeaseToken, 1002, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("Mark external sent: %v", err)
	}
	if _, err := value.FindRecentSentStickerAsset(ctx, fixture.event.SessionID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("external latest sticker resolved error=%v", err)
	}
	if _, err := value.FindRecentSentStickerAsset(ctx, "other-session"); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("other session resolved error=%v", err)
	}
}

func TestStickerLibraryReplacesDescriptionAndDeletesCascades(t *testing.T) {
	value := openStore(t)
	ctx := context.Background()
	asset := storeTestSticker(t, value, []byte("replace-sticker"), "旧描述")
	label := domain.StickerLabel{
		StickerID: asset.ID, Description: "全新含义", DescriptionNorm: "全新含义",
		SourceSessionID: "private:owner", CollectedByID: "owner", CreatedAt: time.Now(),
	}
	if err := value.ReplaceStickerDescription(ctx, label, []domain.StickerSearchTerm{{Value: "new-term", Weight: 100}}); err != nil {
		t.Fatalf("ReplaceStickerDescription: %v", err)
	}
	oldMatches, err := value.SearchStickerLibrary(ctx, []domain.StickerSearchTerm{{Value: "old-term", Weight: 100}}, 5)
	if err != nil || len(oldMatches) != 0 {
		t.Fatalf("old search=%#v err=%v", oldMatches, err)
	}
	newMatches, err := value.SearchStickerLibrary(ctx, []domain.StickerSearchTerm{{Value: "new-term", Weight: 100}}, 5)
	if err != nil || len(newMatches) != 1 || newMatches[0].Description != "全新含义" {
		t.Fatalf("new search=%#v err=%v", newMatches, err)
	}
	if err := value.DeleteStickerAsset(ctx, asset.ID); err != nil {
		t.Fatalf("DeleteStickerAsset: %v", err)
	}
	if _, err := value.GetStickerAsset(ctx, asset.ID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("deleted asset error=%v", err)
	}
	items, total, err := value.StickerLibraryInventory(ctx, 20, 0)
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("inventory items=%#v total=%d err=%v", items, total, err)
	}
	newMatches, err = value.SearchStickerLibrary(ctx, []domain.StickerSearchTerm{{Value: "new-term", Weight: 100}}, 5)
	if err != nil || len(newMatches) != 0 {
		t.Fatalf("deleted search=%#v err=%v", newMatches, err)
	}
}

func storeTestSticker(t *testing.T, value *sqlite.Store, data []byte, description string) domain.StickerAsset {
	t.Helper()
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	asset := domain.StickerAsset{
		ID: "stkl_" + digest, MIMEType: "image/png", Path: "/tmp/" + digest + ".png",
		Size: int64(len(data)), SHA256: digest, CreatedAt: time.Now(),
	}
	label := domain.StickerLabel{
		StickerID: asset.ID, Description: description, DescriptionNorm: description,
		CreatedAt: time.Now(),
	}
	if _, err := value.StoreStickerCollection(context.Background(), asset, label,
		[]domain.StickerSearchTerm{{Value: "old-term", Weight: 100}}, 1<<20); err != nil {
		t.Fatalf("StoreStickerCollection: %v", err)
	}
	return asset
}
