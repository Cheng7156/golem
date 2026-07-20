package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type cronVideoDelivery struct {
	binding domain.CronDeliveryBinding
	direct  domain.CronDirectOutputCommit
	wakes   int
}

func (c *cronVideoDelivery) RegisterCronDelivery(
	context.Context,
	domain.CronDeliveryRegistration,
) (domain.CronDeliveryBinding, error) {
	return c.binding, nil
}

func (*cronVideoDelivery) CommitCronDelivery(
	context.Context,
	domain.CronDeliveryCommit,
) (domain.CronDeliveryResult, error) {
	return domain.CronDeliveryResult{}, nil
}

func (c *cronVideoDelivery) GetCronDelivery(
	context.Context,
	string,
	string,
) (domain.CronDeliveryBinding, error) {
	return c.binding, nil
}

func (c *cronVideoDelivery) CommitCronDirectOutput(
	_ context.Context,
	commit domain.CronDirectOutputCommit,
) (domain.AsyncDirectOutputResult, error) {
	c.direct = commit
	return domain.AsyncDirectOutputResult{
		Queued: true, OutboxID: "outbox-cron-video", Sequence: 21, DirectOutputCount: 1,
	}, nil
}

func (*cronVideoDelivery) CountCronDirectOutputs(
	context.Context,
	domain.CronDirectOutputScope,
) (int64, error) {
	return 1, nil
}

func TestCronVideoSearchAndSendUsePersistedBinding(t *testing.T) {
	delivery := &cronVideoDelivery{binding: domain.CronDeliveryBinding{
		ID: "cdb-video", Profile: "default", JobID: "job-video",
		ChatID: "private:wxid-owner|job", SessionID: "private:wxid-owner",
		ReceiverID: "wxid-owner", Binding: domain.ChannelBinding{
			Channel: "wechat", SessionID: "private:wxid-owner", ReceiverID: "wxid-owner",
			Principal: domain.Principal{ID: "wxid-owner", Name: "Owner"},
		},
	}}
	videos := new(asyncVideoCapability)
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken, CronDelivery: delivery, Videos: videos,
		AsyncDeliveryWake: func() { delivery.wakes++ },
	})
	if err != nil {
		t.Fatalf("NewRelayGateway: %v", err)
	}
	gateway.runtimeCtx = context.Background()
	server := newCronVideoServer(gateway)
	defer server.Close()
	bound := cronBoundRequest{
		Profile: "default", JobID: "job-video", DeliveryID: "job-video:scheduled",
	}
	status, body := postCapability(t, server.URL+cronVideoSearchPath, cronVideoSearchRequest{
		cronBoundRequest: bound, Category: "funny", Limit: 1,
	})
	if status != http.StatusOK || body["candidates"] == nil {
		t.Fatalf("search status=%d body=%#v", status, body)
	}
	status, body = postCapability(t, server.URL+cronVideoSendPath, cronVideoSendRequest{
		cronBoundRequest: bound, CandidateID: "video-candidate", InvocationID: "call-video-1",
	})
	if status != http.StatusAccepted || body["job_id"] == "" {
		t.Fatalf("send status=%d body=%#v", status, body)
	}
	result := waitCronVideoJob(t, cronVideoWait{
		baseURL: server.URL, bound: bound, jobID: body["job_id"].(string),
	})
	if result["queued"] != true || result["outbox_id"] != "outbox-cron-video" {
		t.Fatalf("status body=%#v", result)
	}
	if delivery.direct.InvocationID != "call-video-1" || delivery.wakes != 1 {
		t.Fatalf("commit=%#v wakes=%d", delivery.direct, delivery.wakes)
	}
	if videos.searchScope.ChatID != delivery.binding.ChatID ||
		videos.selectScope.RunID != "cron:cdb-video:job-video:scheduled" {
		t.Fatalf("search=%#v select=%#v", videos.searchScope, videos.selectScope)
	}
}

func newCronVideoServer(gateway *RelayGateway) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(cronVideoSearchPath, gateway.serveCronVideoSearch)
	mux.HandleFunc(cronVideoSendPath, gateway.serveCronVideoSend)
	mux.HandleFunc(cronVideoStatusPath, gateway.serveCronVideoStatus)
	return httptest.NewServer(mux)
}

type cronVideoWait struct {
	baseURL string
	bound   cronBoundRequest
	jobID   string
}

func waitCronVideoJob(t *testing.T, input cronVideoWait) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, body := postCapability(t, input.baseURL+cronVideoStatusPath, cronVideoStatusRequest{
			cronBoundRequest: input.bound, JobID: input.jobID,
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%#v", status, body)
		}
		if body["state"] != "pending" {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cron video job did not complete")
	return nil
}
