package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golem_plugin_hermes/internal/domain"
)

func TestParseRelayStickerIntent(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantContent string
		wantKeyword string
	}{
		{name: "plain text", content: "你继续。", wantContent: "你继续。"},
		{
			name: "allowed keyword", content: "这理解真省电。\n" + relayStickerIntentExample,
			wantContent: "这理解真省电。", wantKeyword: "无语",
		},
		{
			name: "disallowed keyword is stripped", content: "挺好。\n[[GOLEM_HERMES_STICKER_INTENT_V1:开心]]",
			wantContent: "挺好。",
		},
		{
			name: "first allowed keyword wins", content: "看戏。\n[[GOLEM_HERMES_STICKER_INTENT_V1:看戏]]\n[[GOLEM_HERMES_STICKER_INTENT_V1:无语]]",
			wantContent: "看戏。", wantKeyword: "看戏",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, keyword := parseRelayStickerIntent(test.content)
			if content != test.wantContent || keyword != test.wantKeyword {
				t.Fatalf("parse=%q/%q, want %q/%q", content, keyword, test.wantContent, test.wantKeyword)
			}
		})
	}
}

func TestRelayDescriptorAdvertisesStickerIntentOnlyWithCapability(t *testing.T) {
	withStickers := relayDescriptor(relayDescriptorOptions{stickers: true})["platform_hint"].(string)
	withoutStickers := relayDescriptor(relayDescriptorOptions{})["platform_hint"].(string)
	if !strings.Contains(withStickers, relayStickerIntentExample) ||
		!strings.Contains(withStickers, "without another model turn") {
		t.Fatalf("sticker descriptor is missing intent protocol: %q", withStickers)
	}
	if strings.Contains(withoutStickers, "STICKER_INTENT") {
		t.Fatalf("descriptor exposed sticker intent without capability: %q", withoutStickers)
	}
}

func TestStickerIntentAddsEffectWithoutToolRoundTrip(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)

	fixture.sendFinal(t, "这理解真省电。\n"+relayStickerIntentExample)
	events := []Event{
		recvRelayEvent(t, fixture.stream),
		recvRelayEvent(t, fixture.stream),
		recvRelayEvent(t, fixture.stream),
		recvRelayEvent(t, fixture.stream),
	}
	want := []EventKind{EventRunAccepted, EventReplyProposed, EventEffectProposed, EventRunCompleted}
	for index, kind := range want {
		if events[index].Kind != kind {
			t.Fatalf("event %d kind=%s, want %s", index, events[index].Kind, kind)
		}
	}
	if events[1].Text != "这理解真省电。" || strings.Contains(events[1].Text, "STICKER_INTENT") {
		t.Fatalf("visible reply leaked intent: %#v", events[1])
	}
	fixture.stickers.mu.Lock()
	query, selectedID := fixture.stickers.query, fixture.stickers.selectedID
	fixture.stickers.mu.Unlock()
	if query != "无语" || selectedID != "candidate-1" {
		t.Fatalf("sticker query=%q selected=%q", query, selectedID)
	}
}

func TestStickerIntentFailureFallsBackToText(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)
	fixture.stickers.mu.Lock()
	fixture.stickers.searchErr = errors.New("provider unavailable")
	fixture.stickers.mu.Unlock()

	fixture.sendFinal(t, "先把话说明白。\n[[GOLEM_HERMES_STICKER_INTENT_V1:嫌弃]]")
	accepted := recvRelayEvent(t, fixture.stream)
	reply := recvRelayEvent(t, fixture.stream)
	completed := recvRelayEvent(t, fixture.stream)
	if accepted.Kind != EventRunAccepted || reply.Kind != EventReplyProposed ||
		completed.Kind != EventRunCompleted || reply.Text != "先把话说明白。" {
		t.Fatalf("fallback events=%#v %#v %#v", accepted, reply, completed)
	}
}

func TestStickerIntentDoesNotDuplicateStagedEffect(t *testing.T) {
	fixture := newStickerRelayFixture(t, "[direct]\nmessage: hello")
	defer fixture.close(t)
	fixture.selectSticker(t)

	fixture.sendFinal(t, "够了。\n[[GOLEM_HERMES_STICKER_INTENT_V1:闭嘴]]")
	for index, kind := range []EventKind{
		EventRunAccepted, EventReplyProposed, EventEffectProposed, EventRunCompleted,
	} {
		if event := recvRelayEvent(t, fixture.stream); event.Kind != kind {
			t.Fatalf("event %d kind=%s, want %s", index, event.Kind, kind)
		}
	}
	fixture.stickers.mu.Lock()
	query := fixture.stickers.query
	selectedIDs := append([]string(nil), fixture.stickers.selectedIDs...)
	fixture.stickers.mu.Unlock()
	if query != "" || len(selectedIDs) != 1 {
		t.Fatalf("automatic intent duplicated effect: query=%q selected=%v", query, selectedIDs)
	}
}

func TestStickerIntentDoesNotSearchForAmbientRun(t *testing.T) {
	stickers := &fakeStickerCapability{}
	gateway, err := NewRelayGateway(RelayConfig{
		CapabilityToken: testCapabilityToken,
		Stickers:        stickers,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &relayRun{
		request: RunRequest{RunID: "ambient", SessionID: "chatroom:room", TriggerKind: domain.TriggerAmbient},
		chatID:  "chatroom:room|interactive",
	}
	effect, err := gateway.resolveStickerIntent(context.Background(), run, "无语")
	if err != nil || effect != nil {
		t.Fatalf("ambient effect=%#v err=%v", effect, err)
	}
	stickers.mu.Lock()
	query := stickers.query
	stickers.mu.Unlock()
	if query != "" {
		t.Fatalf("ambient searched stickers with query %q", query)
	}
}

func TestV2StickerIntentAddsEffectAndPreservesRawResultHash(t *testing.T) {
	request := RunRequest{
		RunID: "run-sticker-intent-v2", SessionID: "private:owner", Lane: domain.LaneInteractive,
		Input: "发句话", ChatType: "dm", ConversationID: "wechat:private:owner",
		CurrentObservationID: "obs-sticker", CurrentPayloadHash: "payload-sticker", RequiredContextSeq: 1,
		InvocationID: "invoke-sticker-intent", TriggerKind: domain.TriggerExplicit, RequireVisibleReply: true,
	}
	stickers := &fakeStickerCapability{}
	_, results, connection, stream := startV2RelayWithConfig(t, request, RelayConfig{
		CapabilityToken: testCapabilityToken,
		Stickers:        stickers,
	})
	content := "这理解真省电。\n" + relayStickerIntentExample
	proposalID := "proposal-sticker-intent"
	hash := resultHash(t, request.InvocationID, proposalID, "visible_reply", content, []OutputProposal{})
	writeRelayFrame(t, connection, map[string]any{
		"type": "outbound", "requestId": "request-sticker-intent",
		"action": map[string]any{
			"op": "commit_run_result_v1", "invocation_id": request.InvocationID,
			"proposal_id": proposalID, "result_kind": "visible_reply", "content": content,
			"effects": []any{}, "result_hash": hash,
		},
	})
	reply, err := stream.Recv(context.Background())
	if err != nil || reply.Kind != EventReplyProposed || reply.Text != "这理解真省电。" {
		t.Fatalf("reply=%#v err=%v", reply, err)
	}
	effect, err := stream.Recv(context.Background())
	if err != nil || effect.Kind != EventEffectProposed || effect.Proposal == nil ||
		effect.Proposal.Kind != "emoji" {
		t.Fatalf("effect=%#v err=%v", effect, err)
	}
	completed, err := stream.Recv(context.Background())
	if err != nil || completed.Kind != EventRunCompleted || completed.ResultHash != hash {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	results.put(domain.RelayRunResult{
		ProposalID: proposalID, InvocationID: request.InvocationID, RunID: request.RunID,
		ResultKind: "visible_reply", ResultHash: hash, OutboxIDs: []string{"outbox-sticker"},
	})
	if err := stream.Send(context.Background(), Command{
		Kind: CommandProposalResult, RunID: request.RunID, ProposalID: proposalID,
	}); err != nil {
		t.Fatal(err)
	}
	result := readRelayFrame(t, connection)
	body := result["result"].(map[string]any)
	if body["success"] != true || body["result_hash"] != hash {
		t.Fatalf("result=%#v", result)
	}
	stickers.mu.Lock()
	query, selectedID := stickers.query, stickers.selectedID
	stickers.mu.Unlock()
	if query != "无语" || selectedID != "candidate-1" {
		t.Fatalf("sticker query=%q selected=%q", query, selectedID)
	}
}
