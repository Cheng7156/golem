package main

import (
	"testing"

	"golem_plugin_hermes/internal/config"
)

func TestOnConfigChangePublishesRuntimeRoutingConfig(t *testing.T) {
	p := newHermesPlugin()
	initial := config.Default()
	initial.Routing.DecisionBaseURL = "https://example.com/v1"
	initial.Routing.DecisionModel = "test-model"
	manager, err := config.NewManager(initial)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p.config = manager
	p.Config = initial
	p.Config.Routing.SocialMode = "hybrid"
	p.Config.Routing.SampleRate = 0.25

	if err := p.OnConfigChange(); err != nil {
		t.Fatalf("OnConfigChange: %v", err)
	}
	snapshot := manager.Current()
	if snapshot.Routing.SocialMode != "hybrid" || snapshot.Routing.SampleRate != 0.25 {
		t.Fatalf("runtime config was not published: %#v", snapshot.Routing)
	}
}

func TestOnConfigChangeRejectsSocialDeciderStaticChanges(t *testing.T) {
	p := newHermesPlugin()
	initial := config.Default()
	initial.Routing.DecisionBaseURL = "https://example.com/v1"
	initial.Routing.DecisionModel = "test-model"
	manager, err := config.NewManager(initial)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p.config = manager
	p.Config = initial
	p.Config.Routing.DecisionModel = "new-model"

	if err := p.OnConfigChange(); err == nil {
		t.Fatal("SocialDecider static config change was accepted without reload")
	}
}

func TestOnConfigChangeRejectsContextProtocolChanges(t *testing.T) {
	p := newHermesPlugin()
	initial := config.Default()
	manager, err := config.NewManager(initial)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p.config = manager
	p.Config = initial
	p.Config.Context.Mode = "full"

	if err := p.OnConfigChange(); err == nil {
		t.Fatal("context protocol change was accepted without plugin reload")
	}
}

func TestOnConfigChangeRejectsSchedulerChanges(t *testing.T) {
	p := newHermesPlugin()
	initial := config.Default()
	manager, err := config.NewManager(initial)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p.config = manager
	p.Config = initial
	p.Config.Scheduler.RunAdmissionMode = "active"

	if err := p.OnConfigChange(); err == nil {
		t.Fatal("scheduler change was accepted without plugin reload")
	}
}

func TestOnConfigChangeRejectsInvalidRuntimeConfig(t *testing.T) {
	p := newHermesPlugin()
	p.Config = config.Default()
	p.Config.Routing.SocialMode = "invalid"

	if err := p.OnConfigChange(); err == nil {
		t.Fatal("invalid config was accepted")
	}
}
