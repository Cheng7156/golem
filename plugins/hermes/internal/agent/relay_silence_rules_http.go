package agent

import (
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const silenceRuleAddPath = "/capabilities/v1/silence-rules/add"

type silenceRuleAddRequest struct {
	MatchType string                   `json:"match_type"`
	Value     string                   `json:"value"`
	Context   capabilitySessionContext `json:"context"`
}

func (g *RelayGateway) serveSilenceRuleAdd(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input silenceRuleAddRequest
	if err := decodeCapabilityRequest(w, request, &input); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid silence rule request")
		return
	}
	run, err := g.capabilityRun(input.Context)
	if err != nil {
		writeCapabilityError(w, http.StatusConflict, err.Error())
		return
	}
	if !run.request.Principal.IsOwner {
		writeCapabilityError(w, http.StatusForbidden, "only the owner may add silence rules")
		return
	}
	if run.request.Lane != domain.LaneInteractive || run.request.TriggerKind != domain.TriggerExplicit {
		writeCapabilityError(w, http.StatusForbidden, "silence rules require an explicit interactive request")
		return
	}
	rule, line, err := normalizeManagedSilenceRule(input.MatchType, input.Value)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return
	}

	g.silenceRulesMu.Lock()
	created, err := appendSilenceRule(g.config.SilenceRulesFile, rule, line)
	g.silenceRulesMu.Unlock()
	if err != nil {
		writeCapabilityError(w, http.StatusInternalServerError, "could not persist silence rule")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{
		"applied": true, "created": created, "match_type": rule.kind,
		"value": strings.TrimSpace(input.Value), "rule": line,
	})
}
