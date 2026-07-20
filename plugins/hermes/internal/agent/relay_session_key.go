package agent

import "strings"

const defaultRelaySessionNamespace = "agent:main"

type relaySessionIdentity struct {
	request RunRequest
	chatID  string
	profile string
}

func relaySessionKey(request RunRequest, chatID string) string {
	return relaySessionKeyBase(defaultRelaySessionNamespace, request, chatID)
}

func (i relaySessionIdentity) matches(actual string) bool {
	for _, expected := range relaySessionKeyCandidates(i.request, i.chatID, i.profile) {
		if actual == expected {
			return true
		}
	}
	return false
}

func relaySessionKeyCandidates(
	request RunRequest,
	chatID string,
	profile string,
) []string {
	namespaces := []string{defaultRelaySessionNamespace}
	if namespace := relayProfileSessionNamespace(profile); namespace != "" {
		namespaces = append(namespaces, namespace)
	}
	result := make([]string, 0, len(namespaces)*2)
	for _, namespace := range namespaces {
		base := relaySessionKeyBase(namespace, request, chatID)
		result = append(result, base)
		if participant := relayGroupParticipant(request); participant != "" {
			result = append(result, base+":"+participant)
		}
	}
	return result
}

func relaySessionKeyBase(namespace string, request RunRequest, chatID string) string {
	chatType := strings.TrimSpace(request.ChatType)
	if chatType == "" || chatType == "dm" {
		chatType = "dm"
	}
	return namespace + ":relay:" + chatType + ":" + chatID
}

func relayGroupParticipant(request RunRequest) string {
	chatType := strings.TrimSpace(request.ChatType)
	if chatType == "" || chatType == "dm" {
		return ""
	}
	return strings.TrimSpace(request.Principal.ID)
}

func relayProfileSessionNamespace(profile string) string {
	value := strings.TrimSpace(profile)
	if value == "" || value == "default" {
		return ""
	}
	return "agent:" + value
}
