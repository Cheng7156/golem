package execution

import (
	"encoding/json"
	"testing"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/domain"
)

func TestValidateVideoOutput(t *testing.T) {
	payload, err := json.Marshal(domain.VideoOutput{
		ObjectID: "video-object", ThumbObjectID: "thumb-object", Duration: 12,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := validateOutputPayload(agent.OutputProposal{Kind: "video", Payload: payload}); err != nil {
		t.Fatalf("validateOutputPayload: %v", err)
	}
}

func TestValidateVideoOutputRequiresObjectsAndDuration(t *testing.T) {
	tests := []domain.VideoOutput{
		{ThumbObjectID: "thumb", Duration: 1},
		{ObjectID: "video", Duration: 1},
		{ObjectID: "video", ThumbObjectID: "thumb"},
	}
	for _, value := range tests {
		payload, _ := json.Marshal(value)
		if err := validateOutputPayload(agent.OutputProposal{Kind: "video", Payload: payload}); err == nil {
			t.Fatalf("accepted invalid video output: %#v", value)
		}
	}
}
