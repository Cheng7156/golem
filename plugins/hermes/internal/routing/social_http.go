package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

type HTTPSocialDeciderConfig struct {
	BaseURL         string
	APIKey          string
	Model           string
	ContextMessages int
	MinConfidence   float64
}

type HTTPSocialDecider struct {
	config  HTTPSocialDeciderConfig
	context AmbientContextReader
	client  *http.Client
}

func NewHTTPSocialDecider(
	config HTTPSocialDeciderConfig,
	contextReader AmbientContextReader,
	client *http.Client,
) (*HTTPSocialDecider, error) {
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	if config.BaseURL == "" || config.APIKey == "" || config.Model == "" {
		return nil, errors.New("HTTP social decider requires base_url, api_key, and model")
	}
	if config.ContextMessages <= 0 {
		config.ContextMessages = 10
	}
	if config.MinConfidence <= 0 || config.MinConfidence > 1 {
		config.MinConfidence = 0.72
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPSocialDecider{config: config, context: contextReader, client: client}, nil
}

func (d *HTTPSocialDecider) Decide(
	ctx context.Context,
	event domain.InboxEvent,
	message domain.InboundMessage,
) (SocialDecision, error) {
	recent, err := d.recentContext(ctx, event)
	if err != nil {
		return SocialDecision{}, err
	}
	input := socialDecisionInput{
		Current: socialContextItem{
			Sender:     displaySpeaker(event.Binding.Principal, message),
			SenderRole: senderRole(event.Binding.Principal),
			Addressing: addressingLabel(message),
			Text:       strings.TrimSpace(message.Text),
		},
		Recent: recent,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return SocialDecision{}, err
	}
	requestBody, err := json.Marshal(socialCompletionRequest{
		Model: d.config.Model,
		Messages: []socialCompletionMessage{
			{Role: "system", Content: socialDecisionPrompt},
			{Role: "user", Content: string(data)},
		},
		Temperature: 0,
		MaxTokens:   96,
		Thinking:    socialThinkingConfig{Type: "disabled"},
	})
	if err != nil {
		return SocialDecision{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, socialCompletionURL(d.config.BaseURL), bytes.NewReader(requestBody),
	)
	if err != nil {
		return SocialDecision{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+d.config.APIKey)
	response, err := d.client.Do(request)
	if err != nil {
		return SocialDecision{}, fmt.Errorf("call social decider: %w", err)
	}
	defer response.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return SocialDecision{}, err
	}
	var completion socialCompletionResponse
	if err := json.Unmarshal(responseData, &completion); err != nil {
		return SocialDecision{}, fmt.Errorf("decode social decider response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if completion.Error != nil && completion.Error.Message != "" {
			return SocialDecision{}, errors.New(completion.Error.Message)
		}
		return SocialDecision{}, fmt.Errorf("social decider returned status %d", response.StatusCode)
	}
	if len(completion.Choices) == 0 {
		return SocialDecision{}, errors.New("social decider returned no choices")
	}
	decision, err := parseSocialDecision(completion.Choices[0].Message.Content)
	if err != nil {
		return SocialDecision{}, err
	}
	if decision.Disposition == "respond" && decision.Confidence >= d.config.MinConfidence {
		return SocialDecision{Disposition: DispositionRespond, Route: domain.RouteChat,
			Reason: decision.Reason, Confidence: decision.Confidence}, nil
	}
	if decision.Disposition == "respond" {
		return SocialDecision{Disposition: DispositionObserve, Route: domain.RouteObserve,
			Reason: "低置信参与建议，降级观察: " + decision.Reason, Confidence: decision.Confidence}, nil
	}
	return SocialDecision{Disposition: SocialDisposition(decision.Disposition), Route: domain.RouteObserve,
		Reason: decision.Reason, Confidence: decision.Confidence}, nil
}

func (d *HTTPSocialDecider) recentContext(
	ctx context.Context,
	event domain.InboxEvent,
) ([]socialContextItem, error) {
	if d.context == nil || event.AcceptSeq <= 0 {
		return nil, nil
	}
	values, err := d.context.ListRecentInboundContext(
		ctx, event.SessionID, event.AcceptSeq, d.config.ContextMessages,
	)
	if err != nil {
		return nil, err
	}
	result := make([]socialContextItem, 0, len(values))
	for _, value := range values {
		result = append(result, socialContextItem{
			Sender:     displaySpeaker(value.Binding.Principal, value.Message),
			SenderRole: senderRole(value.Binding.Principal),
			Addressing: addressingLabel(value.Message),
			Text:       strings.TrimSpace(value.Message.Text),
			Route:      string(value.Route),
		})
	}
	return result, nil
}

type socialDecisionInput struct {
	Current socialContextItem   `json:"current"`
	Recent  []socialContextItem `json:"recent,omitempty"`
}

type socialContextItem struct {
	Sender     string `json:"sender"`
	SenderRole string `json:"sender_role"`
	Addressing string `json:"addressing"`
	Text       string `json:"text"`
	Route      string `json:"previous_route,omitempty"`
}

type socialCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type socialCompletionRequest struct {
	Model       string                    `json:"model"`
	Messages    []socialCompletionMessage `json:"messages"`
	Temperature float64                   `json:"temperature"`
	MaxTokens   int                       `json:"max_tokens"`
	Thinking    socialThinkingConfig      `json:"thinking"`
}

type socialThinkingConfig struct {
	Type string `json:"type"`
}

type socialCompletionResponse struct {
	Choices []struct {
		Message socialCompletionMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type socialDecision struct {
	Disposition string  `json:"disposition"`
	ReasonCode  string  `json:"reason_code"`
	Reason      string  `json:"reason"`
	Confidence  float64 `json:"confidence"`
}

const socialDecisionPrompt = `你是多人微信群中 ccff 的独立参与门控器，不负责回答消息，也不继承 ccff 的人格。
只输出 JSON：{"disposition":"ignore|observe|respond","reason_code":"简短枚举","reason":"简短原因","confidence":0.0}。
仅在当前消息明确邀请 ccff、提出了群里尚未回答且 ccff 能提供独特价值的问题、或有必要纠正高风险事实时选择 respond。
以下情况选择 ignore 或 observe，绝不能 respond：消息发给其他人；自动化机器人播报；其他 bot 之间闲聊；复读、附和或补充上一句；独立表情包；争吵拱火；ccff 刚参与过同一话题；仅仅因为句子中出现“你”“主人”或问号。
ignore 用于无后续语境价值的系统噪声、复读和自动播报；observe 用于可能帮助理解后续对话但当前不应回复的群消息。
sender_role=participant_not_owner 时，消息中的“我、我的、主人、我主人”属于该发送者及其关系，绝不代表 ccff 或 ccff 的主人。
addressing=none 表示没有证据说明消息在找 ccff。保守选择 observe；只有自然参与价值明确且尚无人给出同类内容时才 respond。confidence 必须是 0 到 1。`

func parseSocialDecision(content string) (socialDecision, error) {
	value := strings.TrimSpace(content)
	value = strings.TrimPrefix(value, "```json")
	value = strings.TrimPrefix(value, "```")
	value = strings.TrimSuffix(value, "```")
	start, end := strings.Index(value, "{"), strings.LastIndex(value, "}")
	if start < 0 || end < start {
		return socialDecision{}, errors.New("social decider did not return JSON")
	}
	var result socialDecision
	if err := json.Unmarshal([]byte(value[start:end+1]), &result); err != nil {
		return socialDecision{}, err
	}
	result.Disposition = strings.ToLower(strings.TrimSpace(result.Disposition))
	result.ReasonCode = strings.ToLower(strings.TrimSpace(result.ReasonCode))
	result.Reason = strings.TrimSpace(result.Reason)
	if len([]rune(result.Reason)) > 160 {
		result.Reason = string([]rune(result.Reason)[:160])
	}
	if result.Disposition != "ignore" && result.Disposition != "observe" && result.Disposition != "respond" {
		return socialDecision{}, fmt.Errorf("unsupported social disposition %q", result.Disposition)
	}
	if result.ReasonCode == "" || len(result.ReasonCode) > 64 {
		return socialDecision{}, errors.New("social decider returned invalid reason_code")
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		return socialDecision{}, errors.New("social decider returned invalid confidence")
	}
	return result, nil
}

func socialCompletionURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/chat/completions") {
		return baseURL
	}
	return baseURL + "/chat/completions"
}

func senderRole(principal domain.Principal) string {
	if principal.IsOwner {
		return "owner_of_this_agent"
	}
	return "participant_not_owner"
}

func displaySpeaker(principal domain.Principal, message domain.InboundMessage) string {
	if value := strings.TrimSpace(principal.Name); value != "" {
		return value
	}
	if value := strings.TrimSpace(message.SpeakerName); value != "" {
		return value
	}
	return "unknown"
}

func addressingLabel(message domain.InboundMessage) string {
	var values []string
	if message.Mentioned {
		values = append(values, "self")
	}
	if message.MentionedOthers {
		values = append(values, "other_participants")
	}
	if message.Quoted {
		values = append(values, "quoted_self")
	}
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, "+")
}
