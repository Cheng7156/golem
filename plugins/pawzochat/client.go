package main

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxBridgeResponseBytes = 40 * 1024 * 1024
	noReplyMarker          = "[[PAWZOCHAT_NO_REPLY]]"
	legacyNoReplyMarker    = "[PAWZOCHAT_NO_REPLY]"
	bareNoReplyMarker      = "PAWZOCHAT_NO_REPLY"
)

var noReplyMarkerWrappers = []string{"***", "___", "**", "__", "~~", "`", "*", "_"}

type bridgeRequest struct {
	PersonaID      string `json:"persona_id"`
	SessionKey     string `json:"session_key"`
	SessionName    string `json:"session_name,omitempty"`
	Text           string `json:"text"`
	Quote          string `json:"quote,omitempty"`
	ForceReply     bool   `json:"force_reply"`
	RequestID      string `json:"request_id"`
	DeadlineUnixMS int64  `json:"deadline_unix_ms"`
}

type bridgeResponse struct {
	PersonaID string          `json:"persona_id"`
	Messages  []bridgeMessage `json:"messages"`
	Outcome   string          `json:"outcome"`
	Error     string          `json:"error"`
}

type bridgeMessage struct {
	Content []bridgeBlock `json:"content"`
}

type bridgeBlock struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	Data       string `json:"data"`
	Name       string `json:"name"`
	DurationMS uint32 `json:"duration_ms"`
	DeliveryID string `json:"delivery_id"`
}

type outbound struct {
	Kind       string
	Text       string
	Data       []byte
	DurationMS uint32
	PersonaID  string
	DeliveryID string
}

func (p *PawzoChatPlugin) requestReply(
	config Config,
	personaID string,
	incoming incomingMessage,
) ([]outbound, bool, error) {
	return p.requestReplyPrompt(
		config, personaID, incoming.SessionKey, incoming.sessionName(),
		incoming.promptContent(), incoming.Quote.Content, incoming.isExplicit(),
	)
}

func (p *PawzoChatPlugin) requestReplyPrompt(
	config Config,
	personaID string,
	sessionKey string,
	sessionName string,
	prompt string,
	quote string,
	forceReply bool,
) ([]outbound, bool, error) {
	requestID := newBridgeRequestID()
	deadline := time.Now().Add(time.Duration(config.HTTPTimeoutSeconds) * time.Second)
	payload, err := json.Marshal(bridgeRequest{
		PersonaID:      personaID,
		SessionKey:     sessionKey,
		SessionName:    sessionName,
		Text:           prompt,
		Quote:          quote,
		ForceReply:     forceReply,
		RequestID:      requestID,
		DeadlineUnixMS: deadline.UnixMilli(),
	})
	if err != nil {
		return nil, false, err
	}
	url := strings.TrimRight(config.BaseURL, "/") + "/api/bridge/golem/messages"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("build PawzoChat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+config.Token)
	}
	client := &http.Client{Timeout: time.Duration(config.HTTPTimeoutSeconds) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		p.cancelRequest(config, requestID)
		return nil, false, fmt.Errorf("call PawzoChat: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBridgeResponseBytes+1))
	if err != nil {
		p.cancelRequest(config, requestID)
		return nil, false, fmt.Errorf("read PawzoChat response: %w", err)
	}
	if len(body) > maxBridgeResponseBytes {
		return nil, false, errors.New("PawzoChat response exceeds 40 MiB")
	}
	var decoded bridgeResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		p.cancelRequest(config, requestID)
		return nil, false, fmt.Errorf("decode PawzoChat response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if decoded.Error == "" {
			decoded.Error = resp.Status
		}
		return nil, false, fmt.Errorf("PawzoChat request failed: %s", decoded.Error)
	}
	if decoded.Outcome == "no_reply" {
		if len(decoded.Messages) != 0 {
			return nil, false, errors.New("PawzoChat no_reply response contains messages")
		}
		return nil, true, nil
	}
	if decoded.Outcome != "" && decoded.Outcome != "replied" {
		return nil, false, fmt.Errorf("unknown PawzoChat outcome: %s", decoded.Outcome)
	}
	outputs, err := decoded.outputs()
	if err != nil {
		return nil, false, err
	}
	outputs, markerRemoved := filterNoReplyMarkerOutputs(outputs)
	if markerRemoved && len(outputs) == 0 {
		return nil, true, nil
	}
	for index := range outputs {
		if outputs[index].PersonaID == "" {
			outputs[index].PersonaID = personaID
		}
	}
	return limitOutboundText(prepareOutboundText(outputs)), false, nil
}

func filterNoReplyMarkerOutputs(outputs []outbound) ([]outbound, bool) {
	filtered := make([]outbound, 0, len(outputs))
	markerRemoved := false
	for _, output := range outputs {
		if output.Kind != "text" {
			filtered = append(filtered, output)
			continue
		}
		if isNoReplyMarker(output.Text) {
			markerRemoved = true
			continue
		}
		lines := strings.Split(strings.ReplaceAll(output.Text, "\r\n", "\n"), "\n")
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			if isNoReplyMarker(line) {
				markerRemoved = true
				continue
			}
			kept = append(kept, line)
		}
		output.Text = strings.TrimSpace(strings.Join(kept, "\n"))
		if output.Text != "" {
			filtered = append(filtered, output)
		}
	}
	return filtered, markerRemoved
}

func isNoReplyMarker(text string) bool {
	candidate := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
			return -1
		default:
			return r
		}
	}, text))
	for candidate != "" {
		if len(candidate) >= 6 &&
			(strings.HasPrefix(candidate, "```") || strings.HasPrefix(candidate, "~~~")) &&
			strings.HasSuffix(candidate, candidate[:3]) {
			candidate = strings.TrimSpace(candidate[3 : len(candidate)-3])
			if language, body, found := strings.Cut(candidate, "\n"); found && isNoReplyCodeFenceLanguage(language) {
				candidate = strings.TrimSpace(body)
			}
			continue
		}
		unwrapped := false
		for _, wrapper := range noReplyMarkerWrappers {
			if len(candidate) >= len(wrapper)*2 &&
				strings.HasPrefix(candidate, wrapper) &&
				strings.HasSuffix(candidate, wrapper) {
				candidate = strings.TrimSpace(candidate[len(wrapper) : len(candidate)-len(wrapper)])
				unwrapped = true
				break
			}
		}
		if !unwrapped {
			break
		}
	}
	return candidate == noReplyMarker ||
		candidate == legacyNoReplyMarker ||
		candidate == bareNoReplyMarker
}

func isNoReplyCodeFenceLanguage(language string) bool {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "text", "txt", "plaintext", "markdown", "md":
		return true
	default:
		return false
	}
}

func limitOutboundText(outputs []outbound) []outbound {
	limited := make([]outbound, 0, len(outputs))
	textIndexes := make([]int, 0, 3)
	for _, output := range outputs {
		if output.Kind != "text" {
			limited = append(limited, output)
			continue
		}
		if len(textIndexes) < 3 {
			limited = append(limited, output)
			textIndexes = append(textIndexes, len(limited)-1)
			continue
		}
		index := textIndexes[2]
		if limited[index].Text == "" {
			limited[index].Text = output.Text
		} else {
			limited[index].Text += "\n" + output.Text
		}
	}
	return limited
}

func newBridgeRequestID() string {
	var raw [12]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func (p *PawzoChatPlugin) cancelRequest(config Config, requestID string) {
	if requestID == "" {
		return
	}
	endpoint := strings.TrimRight(config.BaseURL, "/") +
		"/api/bridge/golem/requests/" + url.PathEscape(requestID) + "/cancel"
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+config.Token)
	}
	timeout := time.Duration(config.HTTPTimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 3*time.Second {
		timeout = 3 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("[pawzochat] 取消超时请求失败", "request_id", requestID, "err", err)
		return
	}
	_ = resp.Body.Close()
	slog.Info("[pawzochat] 已取消超时请求", "request_id", requestID, "status", resp.StatusCode)
}

func (p *PawzoChatPlugin) confirmEmojiDelivery(
	config Config,
	personaID string,
	deliveryID string,
) error {
	payload, err := json.Marshal(map[string]string{
		"persona_id":  strings.TrimSpace(personaID),
		"delivery_id": strings.TrimSpace(deliveryID),
	})
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(config.BaseURL, "/") + "/api/bridge/golem/deliveries/emoji"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+config.Token)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("confirm emoji delivery: %s", resp.Status)
	}
	return nil
}

func (response bridgeResponse) outputs() ([]outbound, error) {
	var outputs []outbound
	for _, item := range response.Messages {
		for _, block := range item.Content {
			kind := strings.ToLower(strings.TrimSpace(block.Type))
			switch kind {
			case "text":
				if text := strings.TrimSpace(block.Text); text != "" {
					outputs = append(outputs, outbound{Kind: "text", Text: text})
				}
			case "image", "emoji", "voice":
				if block.Data == "" {
					placeholder := map[string]string{
						"image": "[图片]", "emoji": "[表情]", "voice": "[语音]",
					}[kind]
					outputs = append(outputs, outbound{Kind: "text", Text: placeholder})
					continue
				}
				data, err := base64.StdEncoding.DecodeString(block.Data)
				if err != nil {
					return nil, fmt.Errorf("decode PawzoChat %s: %w", kind, err)
				}
				outputs = append(outputs, outbound{
					Kind: kind, Text: strings.TrimSpace(block.Text), Data: data,
					DurationMS: block.DurationMS,
					PersonaID:  strings.TrimSpace(response.PersonaID),
					DeliveryID: strings.TrimSpace(block.DeliveryID),
				})
			case "file":
				name := strings.TrimSpace(block.Name)
				if name == "" {
					name = "文件"
				}
				outputs = append(outputs, outbound{Kind: "text", Text: "[文件] " + name})
			}
		}
	}
	return outputs, nil
}
