package sqlite

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

func asyncOutputs(commit domain.AsyncDeliveryCommit) ([]domain.AsyncOutput, error) {
	if len(commit.Outputs) > 0 {
		return cloneAsyncOutputs(commit.Outputs), nil
	}
	payload, err := json.Marshal(domain.TextOutput{Content: commit.Content})
	if err != nil {
		return nil, err
	}
	return []domain.AsyncOutput{{Kind: "text", Payload: payload}}, nil
}

func cloneAsyncOutputs(outputs []domain.AsyncOutput) []domain.AsyncOutput {
	values := make([]domain.AsyncOutput, 0, len(outputs))
	for _, output := range outputs {
		output.Payload = cloneBytes(output.Payload)
		values = append(values, output)
	}
	return values
}

func asyncAuditText(commit domain.AsyncDeliveryCommit, outputs []domain.AsyncOutput) string {
	if strings.TrimSpace(commit.Content) != "" {
		return commit.Content
	}
	for _, output := range outputs {
		text, ok := asyncTextOutput(output)
		if ok {
			return text
		}
	}
	return ""
}

func asyncTextOutput(output domain.AsyncOutput) (string, bool) {
	if output.Kind != "text" {
		return "", false
	}
	var text domain.TextOutput
	if json.Unmarshal(output.Payload, &text) != nil {
		return "", false
	}
	return text.Content, true
}

func validateAsyncOutputPayload(output domain.AsyncOutput) error {
	if err := validateAsyncOutputKindPayload(output); err != nil {
		return errors.Join(storeport.ErrInvalid, err)
	}
	return nil
}

func validateAsyncOutputKindPayload(output domain.AsyncOutput) error {
	switch output.Kind {
	case "text":
		return validateAsyncTextOutput(output.Payload)
	case "emoji":
		return validateAsyncEmojiOutput(output.Payload)
	case "image":
		return validateAsyncImageOutput(output.Payload)
	case "video":
		return validateAsyncVideoOutput(output.Payload)
	default:
		return errors.New("unsupported async output kind " + output.Kind)
	}
}

func validateAsyncTextOutput(payload json.RawMessage) error {
	var value domain.TextOutput
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	if strings.TrimSpace(value.Content) == "" {
		return errors.New("async text output is empty")
	}
	return nil
}

func validateAsyncEmojiOutput(payload json.RawMessage) error {
	var value domain.EmojiOutput
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	if len(value.Data) == 0 {
		return errors.New("async emoji output requires inline data")
	}
	return nil
}

func validateAsyncImageOutput(payload json.RawMessage) error {
	var value domain.ImageOutput
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	if len(value.Data) == 0 {
		return errors.New("async image output requires inline data")
	}
	return nil
}

func validateAsyncVideoOutput(payload json.RawMessage) error {
	var value domain.VideoOutput
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	if strings.TrimSpace(value.ObjectID) == "" || strings.TrimSpace(value.ThumbObjectID) == "" {
		return errors.New("async video output requires video and thumbnail objects")
	}
	if value.Duration == 0 {
		return errors.New("async video output requires duration")
	}
	return nil
}

func asyncOutboxID(ticketID string, index int) string {
	if index == 0 {
		return "outbox_" + ticketID
	}
	return "outbox_" + ticketID + "_" + strconv.Itoa(index+1)
}
