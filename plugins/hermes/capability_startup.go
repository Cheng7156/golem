package main

import (
	"fmt"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/mediaobject"
	sqlitestore "golem_plugin_hermes/internal/store/sqlite"
)

type relayCapabilityBundle struct {
	stickers     agent.StickerCapability
	videos       agent.VideoCapability
	mediaObjects mediaobject.Reader
	token        string
}

func buildRelayCapabilityBundle(
	cfg config.Config,
	store *sqlitestore.Store,
) (relayCapabilityBundle, error) {
	stickers, token, err := buildRelayCapabilities(
		cfg.Capabilities,
		cfg.Agent.AsyncDeliveryEnabled,
	)
	if err != nil {
		return relayCapabilityBundle{}, err
	}
	bundle := relayCapabilityBundle{stickers: stickers, token: token}
	if !cfg.Capabilities.Video.Enabled {
		return bundle, nil
	}
	environment, err := loadCapabilityEnvironment(cfg.Capabilities.EnvironmentFile)
	if err != nil {
		return relayCapabilityBundle{}, err
	}
	videos, objects, err := newVideoCapability(
		cfg.Capabilities.Video, capabilityEnvironmentLookup(environment), store,
	)
	if err != nil {
		return relayCapabilityBundle{}, fmt.Errorf("create Hermes video capability: %w", err)
	}
	bundle.videos, bundle.mediaObjects = videos, objects
	return bundle, nil
}

func buildStickerCapability(
	value config.CapabilityConfig,
) (agent.StickerCapability, string, error) {
	return buildRelayCapabilities(value, false)
}

func buildRelayCapabilities(
	value config.CapabilityConfig,
	asyncDeliveryEnabled bool,
) (agent.StickerCapability, string, error) {
	if !value.Sticker.Enabled && !value.Video.Enabled && !asyncDeliveryEnabled {
		return nil, "", nil
	}
	environment, err := loadCapabilityEnvironment(value.EnvironmentFile)
	if err != nil {
		return nil, "", err
	}
	lookup := capabilityEnvironmentLookup(environment)
	token, err := resolveCapabilityToken(value, lookup)
	if err != nil {
		return nil, "", err
	}
	if !value.Sticker.Enabled {
		return nil, token, nil
	}
	stickers, err := newStickerCapability(value.Sticker, lookup)
	if err != nil {
		return nil, "", fmt.Errorf("create Hermes sticker capability: %w", err)
	}
	return stickers, token, nil
}

func asyncDeliveryCapability(
	cfg config.Config,
	store *sqlitestore.Store,
) agent.AsyncDeliveryCapability {
	if !cfg.Agent.AsyncDeliveryEnabled {
		return nil
	}
	return store
}

func cronDeliveryCapability(
	cfg config.Config,
	store *sqlitestore.Store,
) agent.CronDeliveryCapability {
	if !cfg.Agent.AsyncDeliveryEnabled {
		return nil
	}
	return store
}

func signalWake(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}
