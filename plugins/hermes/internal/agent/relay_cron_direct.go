package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

const (
	cronDirectStatusPath = "/capabilities/v1/cron-delivery/status"
	maxCronProfileLength = 128
	maxCronJobIDLength   = 512
	maxCronRunIDLength   = 2048
)

type CronDirectDeliveryCapability interface {
	GetCronDelivery(context.Context, string, string) (domain.CronDeliveryBinding, error)
	CommitCronDirectOutput(
		context.Context,
		domain.CronDirectOutputCommit,
	) (domain.AsyncDirectOutputResult, error)
	CountCronDirectOutputs(context.Context, domain.CronDirectOutputScope) (int64, error)
}

type cronBoundRequest struct {
	Profile    string `json:"profile"`
	JobID      string `json:"job_id"`
	DeliveryID string `json:"delivery_id"`
}

type cronDirectRun struct {
	binding    domain.CronDeliveryBinding
	deliveryID string
	capability CronDirectDeliveryCapability
}

func (g *RelayGateway) serveCronDirectStatus(w http.ResponseWriter, request *http.Request) {
	if !g.prepareCapabilityRequest(w, request) {
		return
	}
	var input cronBoundRequest
	if decodeCapabilityRequest(w, request, &input) != nil {
		writeCapabilityError(w, http.StatusBadRequest, "invalid cron delivery status request")
		return
	}
	run, ok := g.cronDirectRun(w, request, input)
	if !ok {
		return
	}
	count, err := run.capability.CountCronDirectOutputs(request.Context(), run.scope())
	if err != nil {
		writeAsyncStoreError(w, err, "could not read cron direct delivery status")
		return
	}
	writeCapabilityJSON(w, http.StatusOK, map[string]any{"direct_output_count": count})
}

func (g *RelayGateway) cronDirectRun(
	w http.ResponseWriter,
	request *http.Request,
	input cronBoundRequest,
) (cronDirectRun, bool) {
	capability, ok := g.cronDirectCapability()
	if !ok {
		writeCapabilityError(w, http.StatusServiceUnavailable, "cron direct delivery is unavailable")
		return cronDirectRun{}, false
	}
	profile, jobID, deliveryID, err := validateCronBoundRequest(input)
	if err != nil {
		writeCapabilityError(w, http.StatusBadRequest, err.Error())
		return cronDirectRun{}, false
	}
	binding, err := capability.GetCronDelivery(request.Context(), profile, jobID)
	if err != nil {
		writeAsyncStoreError(w, err, "cron delivery binding was not found")
		return cronDirectRun{}, false
	}
	return cronDirectRun{binding: binding, deliveryID: deliveryID, capability: capability}, true
}

func (g *RelayGateway) cronDirectCapability() (CronDirectDeliveryCapability, bool) {
	value, ok := g.config.CronDelivery.(CronDirectDeliveryCapability)
	return value, ok
}

func (r cronDirectRun) scope() domain.CronDirectOutputScope {
	return domain.CronDirectOutputScope{
		Profile: r.binding.Profile, JobID: r.binding.JobID, DeliveryID: r.deliveryID,
	}
}

func validateCronBoundRequest(input cronBoundRequest) (string, string, string, error) {
	profile := strings.TrimSpace(input.Profile)
	jobID := strings.TrimSpace(input.JobID)
	deliveryID := strings.TrimSpace(input.DeliveryID)
	if profile == "" || jobID == "" || deliveryID == "" {
		return "", "", "", errors.New("cron delivery binding is incomplete")
	}
	if len(profile) > maxCronProfileLength || len(jobID) > maxCronJobIDLength ||
		len(deliveryID) > maxCronRunIDLength {
		return "", "", "", errors.New("cron delivery binding is too long")
	}
	return profile, jobID, deliveryID, nil
}
