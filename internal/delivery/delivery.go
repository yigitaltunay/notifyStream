package delivery

import (
	"bytes"
	"context"
	"encoding/json"
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

type OutboundBody struct {
	To       string          `json:"to"`
	Channel  string          `json:"channel"`
	Content  *string         `json:"content,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Priority string          `json:"priority"`
}

func (w *Webhook) Post(ctx context.Context, env amqp.Envelope) (string, error) {
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
		return "", err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("webhook status %d: %s", res.StatusCode, string(respBody))
	}
	var parsed struct {
		MessageID string `json:"messageId"`
	}
	if json.Unmarshal(respBody, &parsed) == nil && parsed.MessageID != "" {
		return parsed.MessageID, nil
	}
	return "", nil
}
