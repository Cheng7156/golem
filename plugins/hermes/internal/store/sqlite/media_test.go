package sqlite_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

func TestVideoOutboxRetainsMediaUntilFinalDelivery(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "video-media")
	ctx := context.Background()
	now := time.Now()
	createTestVideoObjects(t, value, now)
	payload, _ := json.Marshal(domain.VideoOutput{
		ObjectID: "media_video", ThumbObjectID: "media_thumb", Duration: 3,
	})
	items, err := value.CommitRunSuccess(ctx, fixture.run.ID, fixture.run.LeaseToken, []domain.OutboxDraft{{
		SessionID: fixture.event.SessionID, ReceiverID: fixture.event.Binding.ReceiverID,
		Kind: "video", Payload: payload,
	}})
	if err != nil {
		t.Fatalf("CommitRunSuccess: %v", err)
	}
	collectible, _ := value.ListCollectibleMediaObjects(ctx, now, 10)
	if len(collectible) != 0 {
		t.Fatalf("referenced media became collectible: %#v", collectible)
	}
	leased, err := value.LeaseNextOutbox(ctx, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("LeaseNextOutbox: %v", err)
	}
	if leased.ID != items[0].ID {
		t.Fatalf("leased=%s want=%s", leased.ID, items[0].ID)
	}
	if err := value.MarkOutboxSent(ctx, leased.ID, leased.LeaseToken, 1, now); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}
	collectible, err = value.ListCollectibleMediaObjects(ctx, now, 10)
	if err != nil || len(collectible) != 2 {
		t.Fatalf("collectible=%#v error=%v", collectible, err)
	}
}

func TestAsyncDirectVideoRetainsMediaUntilFinalDelivery(t *testing.T) {
	value := openStore(t)
	fixture := createRunningRun(t, value, "direct-video-media")
	ctx := context.Background()
	now := time.Now()
	createTestVideoObjects(t, value, now)
	ticket, err := value.RegisterAsyncDelivery(ctx, asyncRegistration(fixture, "direct-video"))
	if err != nil {
		t.Fatalf("RegisterAsyncDelivery: %v", err)
	}
	commit := directEmojiCommit(t, ticket, "call-video")
	commit.Output = asyncOutput(t, "video", domain.VideoOutput{
		ObjectID: "media_video", ThumbObjectID: "media_thumb", Duration: 3,
	})
	queued, err := value.CommitAsyncDirectOutput(ctx, commit)
	if err != nil || !queued.Queued {
		t.Fatalf("CommitAsyncDirectOutput: result=%#v err=%v", queued, err)
	}
	collectible, _ := value.ListCollectibleMediaObjects(ctx, now, 10)
	if len(collectible) != 0 {
		t.Fatalf("direct video media became collectible: %#v", collectible)
	}
	leased, err := value.LeaseNextOutbox(ctx, now.Add(time.Second), time.Minute)
	if err != nil || leased.ID != queued.OutboxID {
		t.Fatalf("LeaseNextOutbox: item=%#v err=%v", leased, err)
	}
	if err := value.MarkOutboxSent(ctx, leased.ID, leased.LeaseToken, 7, now); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}
	collectible, err = value.ListCollectibleMediaObjects(ctx, now, 10)
	if err != nil || len(collectible) != 2 {
		t.Fatalf("collectible=%#v error=%v", collectible, err)
	}
}

func createTestVideoObjects(t *testing.T, value mediaObjectCreator, now time.Time) {
	t.Helper()
	createTestMediaObject(t, value, domain.MediaObject{
		ID: "media_video", Kind: domain.MediaKindVideo, MIMEType: "video/mp4",
		Path: "video.mp4", Size: 100, SHA256: "video-sha",
		RetainedUntil: now.Add(-time.Minute), CreatedAt: now, LastAccessAt: now,
	})
	createTestMediaObject(t, value, domain.MediaObject{
		ID: "media_thumb", Kind: domain.MediaKindThumbnail, MIMEType: "image/jpeg",
		Path: "thumb.jpg", Size: 10, SHA256: "thumb-sha",
		RetainedUntil: now.Add(-time.Minute), CreatedAt: now, LastAccessAt: now,
	})
}

func createTestMediaObject(t *testing.T, value mediaObjectCreator, object domain.MediaObject) {
	t.Helper()
	stored, created, err := value.CreateMediaObject(context.Background(), object)
	if err != nil || !created || stored.ID != object.ID {
		t.Fatalf("CreateMediaObject: stored=%#v created=%v error=%v", stored, created, err)
	}
}

type mediaObjectCreator interface {
	CreateMediaObject(context.Context, domain.MediaObject) (domain.MediaObject, bool, error)
}
