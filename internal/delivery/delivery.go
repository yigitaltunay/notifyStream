package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yaltunay/notifystream/internal/amqp"
)

type Webhook struct {
	URL    string
	Client *http.Client
}

func NewWebhook(url string) *Webhook {
	return &Webhook{
		URL: url,
		Client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type PermanentError struct {
	Detail string
}

func (e *PermanentError) Error() string { return e.Detail }

type TransientError struct {
	Detail string
}

func (e *TransientError) Error() string { return e.Detail }

type OutboundBody struct {
	To       string          `json:"to"`
	Channel  string          `json:"channel"`
	Content  *string         `json:"content,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Priority string          `json:"priority"`
}

func (w *Webhook) Post(ctx context.Context, env amqp.Envelope) (messageID string, err error) {
	body := OutboundBody{
		To:       env.Recipient,
		Channel:  env.Channel,
		Content:  env.Content,
		Priority: env.Priority,
	}
	if len(env.Payload) > 0 {
		body.Payload = env.Payload
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if env.CorrelationID != nil && *env.CorrelationID != "" {
		req.Header.Set("X-Request-ID", *env.CorrelationID)
	}
	res, err := w.Client.Do(req)
	if err != nil {
		return "", &TransientError{Detail: err.Error()}
	}
	defer func() { _ = res.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		var parsed struct {
			MessageID string `json:"messageId"`
		}
		if json.Unmarshal(respBody, &parsed) == nil && parsed.MessageID != "" {
			return parsed.MessageID, nil
		}
		return "", nil
	case res.StatusCode == http.StatusTooManyRequests:
		return "", &TransientError{Detail: fmt.Sprintf("webhook status %d: %s", res.StatusCode, string(respBody))}
	case res.StatusCode >= 400 && res.StatusCode < 500:
		return "", &PermanentError{Detail: fmt.Sprintf("webhook status %d: %s", res.StatusCode, string(respBody))}
	default:
		return "", &TransientError{Detail: fmt.Sprintf("webhook status %d: %s", res.StatusCode, string(respBody))}
	}
}

func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

func IsTransient(err error) bool {
	var te *TransientError
	return errors.As(err, &te)
}
