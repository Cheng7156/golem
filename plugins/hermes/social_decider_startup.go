package main

import (
	"errors"
	"fmt"
	"strings"

	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/routing"
	sqlitestore "golem_plugin_hermes/internal/store/sqlite"
)

func buildSocialDecider(
	cfg config.Config,
	store *sqlitestore.Store,
) (routing.SocialDecider, error) {
	configured := cfg.Routing.DecisionBaseURL != "" || cfg.Routing.DecisionModel != ""
	if !configured && cfg.Routing.SocialMode != "hybrid" {
		return nil, nil
	}
	if cfg.Routing.DecisionBaseURL == "" || cfg.Routing.DecisionModel == "" {
		return nil, errors.New("routing SocialDecider requires decision_base_url and decision_model")
	}
	environment, err := loadCapabilityEnvironment(cfg.Routing.DecisionEnvironmentFile)
	if err != nil {
		return nil, fmt.Errorf("load social decider environment: %w", err)
	}
	apiKey, _ := capabilityEnvironmentLookup(environment)(cfg.Routing.DecisionAPIKeyEnv)
	apiKey = strings.TrimSpace(apiKey)
	if len(apiKey) < 16 {
		return nil, fmt.Errorf(
			"social decider environment %s must contain at least 16 characters",
			cfg.Routing.DecisionAPIKeyEnv,
		)
	}
	return routing.NewHTTPSocialDecider(routing.HTTPSocialDeciderConfig{
		BaseURL:         cfg.Routing.DecisionBaseURL,
		APIKey:          apiKey,
		Model:           cfg.Routing.DecisionModel,
		ContextMessages: cfg.Routing.DecisionContextMessages,
		MinConfidence:   cfg.Routing.DecisionMinConfidence,
	}, store, nil)
}
