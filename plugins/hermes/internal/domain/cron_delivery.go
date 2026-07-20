package domain

import (
	"errors"
	"strings"
	"time"
)

type CronDeliveryRegistration struct {
	Profile     string
	JobID       string
	ChatID      string
	ParentRunID string
}

func (r CronDeliveryRegistration) Validate() error {
	for _, value := range []string{r.Profile, r.JobID, r.ChatID, r.ParentRunID} {
		if strings.TrimSpace(value) == "" {
			return errors.New("cron delivery registration has an empty binding")
		}
	}
	return nil
}

type CronDeliveryBinding struct {
	ID         string
	Profile    string
	JobID      string
	ChatID     string
	SessionID  string
	ReceiverID string
	Binding    ChannelBinding
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type CronDeliveryCommit struct {
	Profile    string
	JobID      string
	ChatID     string
	DeliveryID string
	Content    string
}

func (c CronDeliveryCommit) Validate() error {
	registration := CronDeliveryRegistration{
		Profile: c.Profile, JobID: c.JobID,
		ChatID: c.ChatID, ParentRunID: "validated-by-binding",
	}
	if err := registration.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.DeliveryID) == "" {
		return errors.New("cron delivery id is empty")
	}
	if strings.TrimSpace(c.Content) == "" {
		return errors.New("cron delivery content is empty")
	}
	return nil
}

type CronDeliveryResult struct {
	Disposition string `json:"disposition"`
	MessageID   string `json:"message_id"`
	OutboxID    string `json:"outbox_id"`
}

type CronDirectOutputCommit struct {
	Profile      string
	JobID        string
	DeliveryID   string
	InvocationID string
	Output       AsyncOutput
}

type CronDirectOutputScope struct {
	Profile    string
	JobID      string
	DeliveryID string
}

func (s CronDirectOutputScope) Validate() error {
	for _, value := range []string{s.Profile, s.JobID, s.DeliveryID} {
		if strings.TrimSpace(value) == "" {
			return errors.New("cron direct output scope has an empty binding")
		}
	}
	return nil
}

func (c CronDirectOutputCommit) Validate() error {
	for _, value := range []string{c.Profile, c.JobID, c.DeliveryID, c.InvocationID} {
		if strings.TrimSpace(value) == "" {
			return errors.New("cron direct output has an empty binding")
		}
	}
	if c.InvocationID != strings.TrimSpace(c.InvocationID) {
		return errors.New("cron direct output invocation id has surrounding whitespace")
	}
	if len(c.InvocationID) > MaxAsyncInvocationIDLength {
		return errors.New("cron direct output invocation id is too long")
	}
	return c.Output.Validate()
}
