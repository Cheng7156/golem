package execution

import (
	"testing"
	"time"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
)

func TestStaleAmbientRun(t *testing.T) {
	cfg, err := config.Normalize(config.Default())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	now := time.Now()
	ambient := domain.Run{TriggerKind: domain.TriggerAmbient}
	message := domain.InboundMessage{OccurredAt: now.Add(-9 * time.Second)}
	if !staleAmbientRun(ambient, message, &config.Snapshot{Config: cfg}, now) {
		t.Fatal("stale ambient Run was not expired")
	}
	message.OccurredAt = now.Add(-time.Second)
	if staleAmbientRun(ambient, message, &config.Snapshot{Config: cfg}, now) {
		t.Fatal("fresh ambient Run was expired")
	}
	ambient.TriggerKind = domain.TriggerExplicit
	message.OccurredAt = now.Add(-time.Hour)
	if staleAmbientRun(ambient, message, &config.Snapshot{Config: cfg}, now) {
		t.Fatal("explicit Run was expired")
	}
}
