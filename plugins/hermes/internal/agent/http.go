package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

type HTTPEngine struct {
	config HTTPConfig
	client *http.Client
}

type completionRequest struct {
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
	Messages []Message `json:"messages"`
}

type completionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func NewHTTPEngine(config HTTPConfig, client *http.Client) (*HTTPEngine, error) {
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	if config.BaseURL == "" || config.Model == "" {
		return nil, errors.New("HTTP compatibility gateway requires base_url and model")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPEngine{config: config, client: client}, nil
}

func (e *HTTPEngine) Start(parent context.Context, request RunRequest) (Stream, error) {
	if strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.Input) == "" {
		return nil, errors.New("Agent Run 缺少 run_id 或 input")
	}
	ctx, cancel := context.WithCancel(parent)
	events := make(chan Event, 4)
	go func() {
		defer close(events)
		events <- Event{Kind: EventRunAccepted, RunID: request.RunID, Sequence: 1, Timestamp: time.Now()}
		reply, err := e.complete(ctx, request)
		if err != nil {
			events <- Event{Kind: EventRunFailed, RunID: request.RunID, Sequence: 2, Timestamp: time.Now(), Err: err}
			return
		}
		if reply != "" {
			proposal, proposalErr := NewTextProposal(reply)
			if proposalErr != nil {
				events <- Event{Kind: EventRunFailed, RunID: request.RunID, Sequence: 2, Timestamp: time.Now(), Err: proposalErr}
				return
			}
			events <- Event{Kind: EventReplyProposed, RunID: request.RunID, Sequence: 2, Timestamp: time.Now(), Text: reply, Proposal: &proposal}
		}
		events <- Event{Kind: EventRunCompleted, RunID: request.RunID, Sequence: 3, Timestamp: time.Now()}
	}()
	return NewChannelStream(cancel, events), nil
}

func (e *HTTPEngine) Health(context.Context) Health {
	return Health{Ready: true, Status: "compatibility"}
}

func (e *HTTPEngine) Close(context.Context) error {
	return nil
}

func (e *HTTPEngine) complete(ctx context.Context, request RunRequest) (string, error) {
	messages := make([]Message, 0, len(request.ConversationSnapshot)+2)
	if prompt := strings.TrimSpace(request.SystemPrompt); prompt != "" {
		messages = append(messages, Message{Role: "system", Content: prompt})
	}
	messages = append(messages, request.ConversationSnapshot...)
	messages = append(messages, Message{Role: "user", Content: request.Input})
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = e.config.Model
	}
	body, err := json.Marshal(completionRequest{Model: model, Messages: messages})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, completionURL(e.config.BaseURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.config.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call HTTP compatibility gateway: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	var result completionResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("decode HTTP compatibility response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if result.Error != nil && result.Error.Message != "" {
			return "", errors.New(result.Error.Message)
		}
		return "", fmt.Errorf("HTTP compatibility gateway returned status %d", resp.StatusCode)
	}
	if len(result.Choices) == 0 {
		return "", errors.New("HTTP compatibility response has no choices")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func completionURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/chat/completions") {
		return baseURL
	}
	return baseURL + "/chat/completions"
}
