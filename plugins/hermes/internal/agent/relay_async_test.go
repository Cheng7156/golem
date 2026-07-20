package agent

import (
	"context"
	"testing"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

type fakeAsyncDeliveryCapability struct{}

func (*fakeAsyncDeliveryCapability) RegisterAsyncDelivery(
	context.Context, domain.AsyncDeliveryRegistration,
) (domain.AsyncDeliveryTicket, error) {
	return domain.AsyncDeliveryTicket{}, nil
}

func (*fakeAsyncDeliveryCapability) GetAsyncDelivery(
	context.Context, string,
) (domain.AsyncDeliveryTicket, error) {
	return domain.AsyncDeliveryTicket{}, storeport.ErrNotFound
}

func (*fakeAsyncDeliveryCapability) CommitAsyncDelivery(
	context.Context, domain.AsyncDeliveryCommit,
) (domain.AsyncDeliveryResult, error) {
	return domain.AsyncDeliveryResult{}, nil
}

func (*fakeAsyncDeliveryCapability) CommitAsyncDirectOutput(
	context.Context, domain.AsyncDirectOutputCommit,
) (domain.AsyncDirectOutputResult, error) {
	return domain.AsyncDirectOutputResult{}, nil
}

func (*fakeAsyncDeliveryCapability) CountAsyncDirectOutputs(
	context.Context, string,
) (int64, error) {
	return 0, nil
}

func (*fakeAsyncDeliveryCapability) RevokeAsyncDeliveries(
	context.Context, string, string, string,
) (int64, error) {
	return 0, nil
}

func (*fakeAsyncDeliveryCapability) ReconcileAsyncDeliveries(
	context.Context, string, string,
) (int64, error) {
	return 0, nil
}

func TestAsyncDeliveryRequiresCapabilityToken(t *testing.T) {
	if _, err := NewRelayGateway(RelayConfig{
		AsyncDelivery: &fakeAsyncDeliveryCapability{},
	}); err == nil {
		t.Fatal("async delivery started without a capability token")
	}
}

func TestAsyncDeliveryRejectsRelayPathCollision(t *testing.T) {
	for _, path := range []string{
		asyncDeliveryRegisterPath,
		asyncDeliveryStatusPath,
		asyncDeliveryDeliverPath,
		asyncDeliveryDeliverV2Path,
		asyncStickerSearchPath,
		asyncStickerSelectPath,
		asyncDeliveryRevokePath,
		asyncDeliveryReconcilePath,
	} {
		if _, err := NewRelayGateway(RelayConfig{
			Path: path, CapabilityToken: testCapabilityToken,
			AsyncDelivery: &fakeAsyncDeliveryCapability{},
		}); err == nil {
			t.Fatalf("expected relay path %q collision", path)
		}
	}
}
