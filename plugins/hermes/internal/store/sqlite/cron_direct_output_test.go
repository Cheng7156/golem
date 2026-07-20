package sqlite_test

import (
	"context"
	"errors"
	"testing"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type cronCommitInput struct {
	binding    domain.CronDeliveryBinding
	deliveryID string
	invocation string
}

func cronEmojiCommit(t *testing.T, input cronCommitInput) domain.CronDirectOutputCommit {
	t.Helper()
	return domain.CronDirectOutputCommit{
		Profile: input.binding.Profile, JobID: input.binding.JobID,
		DeliveryID: input.deliveryID, InvocationID: input.invocation,
		Output: asyncOutput(t, "emoji", domain.EmojiOutput{Data: []byte("gif")}),
	}
}

func TestCronDirectOutputIsIdempotentAndOrdered(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "cron-direct")
	binding, err := value.RegisterCronDelivery(ctx, cronRegistration(fixture))
	if err != nil {
		t.Fatalf("RegisterCronDelivery: %v", err)
	}
	deliveryID := "job_gateway_status:scheduled"
	firstCommit := cronEmojiCommit(t, cronCommitInput{
		binding: binding, deliveryID: deliveryID, invocation: "call-1",
	})
	first, err := value.CommitCronDirectOutput(ctx, firstCommit)
	if err != nil {
		t.Fatalf("CommitCronDirectOutput: %v", err)
	}
	again, err := value.CommitCronDirectOutput(ctx, firstCommit)
	if err != nil {
		t.Fatalf("idempotent direct output: %v", err)
	}
	second, err := value.CommitCronDirectOutput(
		ctx, cronEmojiCommit(t, cronCommitInput{
			binding: binding, deliveryID: deliveryID, invocation: "call-2",
		}),
	)
	if err != nil {
		t.Fatalf("second direct output: %v", err)
	}
	if first.OutboxID != again.OutboxID || first.DirectOutputCount != 1 {
		t.Fatalf("first=%#v again=%#v", first, again)
	}
	if second.Sequence != first.Sequence+1 || second.DirectOutputCount != 2 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	count, err := value.CountCronDirectOutputs(ctx, domain.CronDirectOutputScope{
		Profile: binding.Profile, JobID: binding.JobID, DeliveryID: deliveryID,
	})
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestCronDirectOutputRejectsInvocationReuseWithChangedOutput(t *testing.T) {
	ctx := context.Background()
	value := openStore(t)
	fixture := createRunningRun(t, value, "cron-direct-conflict")
	binding, _ := value.RegisterCronDelivery(ctx, cronRegistration(fixture))
	commit := cronEmojiCommit(t, cronCommitInput{
		binding: binding, deliveryID: "fire-1", invocation: "call-1",
	})
	if _, err := value.CommitCronDirectOutput(ctx, commit); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	commit.Output = asyncOutput(
		t, "emoji", domain.EmojiOutput{Data: []byte("different")},
	)
	if _, err := value.CommitCronDirectOutput(ctx, commit); !errors.Is(err, storeport.ErrConflict) {
		t.Fatalf("changed output err=%v, want conflict", err)
	}
}
