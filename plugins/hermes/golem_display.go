package main

import (
	"strings"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
)

func displayContact(value *contact.Contact) string {
	if value == nil {
		return ""
	}
	for _, candidate := range []string{
		value.GetRemark(), value.GetNickname(), value.GetAlias(), value.GetUsername(),
	} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return ""
}

func displayMember(value interface {
	GetDisplayName() string
	GetRemark() string
	GetNickname() string
	GetAlias() string
	GetUsername() string
}) string {
	if value == nil {
		return ""
	}
	for _, candidate := range []string{
		value.GetDisplayName(), value.GetRemark(), value.GetNickname(),
		value.GetAlias(), value.GetUsername(),
	} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return ""
}

func messageTime(timestamp uint32) time.Time {
	if timestamp == 0 {
		return time.Now()
	}
	return time.Unix(int64(timestamp), 0)
}
