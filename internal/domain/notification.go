package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusQueued    Status = "queued"
	StatusSending   Status = "sending"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type Notification struct {
	ID                uuid.UUID
	BatchID           *uuid.UUID
	Recipient         string
	Channel           Channel
	Content           *string
	TemplateID        *uuid.UUID
	Payload           []byte
	Priority          Priority
	Status            Status
	IdempotencyKey    *string
	ProviderMessageID *string
	ScheduledAt       *time.Time
	CorrelationID     *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type CreateItem struct {
	Recipient      string
	Channel        Channel
	Content        *string
	TemplateID     *uuid.UUID
	Payload        []byte
	Priority       Priority
	IdempotencyKey *string
	ScheduledAt    *time.Time
}

const (
	maxSMSLen       = 1600
	maxEmailSubject = 998
	maxEmailBody    = 256 * 1024
	maxPushTitle    = 50
	maxPushBody     = 200
)

func ValidateCreateItem(i CreateItem) error {
	if strings.TrimSpace(i.Recipient) == "" {
		return fmt.Errorf("recipient is required")
	}
	switch i.Channel {
	case ChannelSMS, ChannelEmail, ChannelPush:
	default:
		return fmt.Errorf("invalid channel")
	}
	switch i.Priority {
	case PriorityHigh, PriorityNormal, PriorityLow, "":
	default:
		return fmt.Errorf("invalid priority")
	}
	hasContent := i.Content != nil && strings.TrimSpace(*i.Content) != ""
	hasTemplate := i.TemplateID != nil
	if !hasContent && !hasTemplate {
		return fmt.Errorf("either content or template_id is required")
	}
	if hasTemplate && (i.Payload == nil || len(i.Payload) == 0) {
		return fmt.Errorf("payload is required when template_id is set")
	}
	if hasContent {
		c := strings.TrimSpace(*i.Content)
		switch i.Channel {
		case ChannelSMS:
			if len(c) > maxSMSLen {
				return fmt.Errorf("sms content exceeds %d characters", maxSMSLen)
			}
		case ChannelEmail:
			lines := strings.SplitN(c, "\n", 2)
			subject := strings.TrimSpace(lines[0])
			body := ""
			if len(lines) > 1 {
				body = lines[1]
			}
			if len(subject) > maxEmailSubject {
				return fmt.Errorf("email subject exceeds %d characters", maxEmailSubject)
			}
			if len(body) > maxEmailBody {
				return fmt.Errorf("email body exceeds %d bytes", maxEmailBody)
			}
		case ChannelPush:
			parts := strings.SplitN(c, "\n", 2)
			title := strings.TrimSpace(parts[0])
			body := ""
			if len(parts) > 1 {
				body = strings.TrimSpace(parts[1])
			}
			if len(title) > maxPushTitle {
				return fmt.Errorf("push title exceeds %d characters", maxPushTitle)
			}
			if len(body) > maxPushBody {
				return fmt.Errorf("push body exceeds %d characters", maxPushBody)
			}
		}
	}
	return nil
}

func QueuePriority(p Priority) uint8 {
	switch p {
	case PriorityHigh:
		return 10
	case PriorityLow:
		return 1
	default:
		return 5
	}
}
