package amqp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/yaltunay/notifystream/internal/domain"
)

const (
	ExchangeTopic = "notifications.topic"
	ExchangeDLX   = "notifications.dlx"
	QueueDLQ      = "q.notify.dlq"
	RoutingDLQ    = "notify.dlq"
)

var queueByChannel = map[domain.Channel]string{
	domain.ChannelSMS:   "q.notify.sms",
	domain.ChannelEmail: "q.notify.email",
	domain.ChannelPush:  "q.notify.push",
}

type Envelope struct {
	ID            string          `json:"id"`
	Recipient     string          `json:"recipient"`
	Channel       string          `json:"channel"`
	Content       *string         `json:"content,omitempty"`
	TemplateID    *string         `json:"template_id,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Priority      string          `json:"priority"`
	CorrelationID *string         `json:"correlation_id,omitempty"`
}

type Client struct {
	url  string
	mu   sync.Mutex
	conn *amqp091.Connection
	ch   *amqp091.Channel
}

func NewClient(url string) *Client {
	return &Client{url: url}
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && !c.conn.IsClosed() {
		return nil
	}
	conn, err := amqp091.DialConfig(c.url, amqp091.Config{
		Heartbeat: 20 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		return fmt.Errorf("amqp dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("amqp channel: %w", err)
	}
	if err := declareTopology(ch); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return err
	}
	if err := ch.Qos(10, 0, false); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("amqp qos: %w", err)
	}
	c.conn = conn
	c.ch = ch
	return nil
}

func declareTopology(ch *amqp091.Channel) error {
	if err := ch.ExchangeDeclare(ExchangeDLX, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dlx: %w", err)
	}
	if _, err := ch.QueueDeclare(QueueDLQ, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dlq: %w", err)
	}
	if err := ch.QueueBind(QueueDLQ, RoutingDLQ, ExchangeDLX, false, nil); err != nil {
		return fmt.Errorf("bind dlq: %w", err)
	}
	if err := ch.ExchangeDeclare(ExchangeTopic, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare topic exchange: %w", err)
	}
	args := amqp091.Table{
		"x-dead-letter-exchange":    ExchangeDLX,
		"x-dead-letter-routing-key": RoutingDLQ,
		"x-max-priority":            int32(10),
	}
	for _, q := range queueByChannel {
		if _, err := ch.QueueDeclare(q, true, false, false, false, args); err != nil {
			return fmt.Errorf("declare queue %s: %w", q, err)
		}
	}
	for chName, q := range queueByChannel {
		for _, pri := range []string{"high", "normal", "low"} {
			key := fmt.Sprintf("notify.%s.%s", chName, pri)
			if err := ch.QueueBind(q, key, ExchangeTopic, false, nil); err != nil {
				return fmt.Errorf("bind %s -> %s: %w", key, q, err)
			}
		}
	}
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if c.ch != nil {
		err = c.ch.Close()
		c.ch = nil
	}
	if c.conn != nil {
		if cerr := c.conn.Close(); err == nil {
			err = cerr
		}
		c.conn = nil
	}
	return err
}

func (c *Client) Ping(ctx context.Context) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || conn.IsClosed() {
		return fmt.Errorf("amqp not connected")
	}
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (c *Client) PublishNotification(ctx context.Context, n domain.Notification) error {
	c.mu.Lock()
	ch := c.ch
	c.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("amqp publish: no channel")
	}
	priority := n.Priority
	if priority == "" {
		priority = domain.PriorityNormal
	}
	routingKey := fmt.Sprintf("notify.%s.%s", n.Channel, priority)
	env := Envelope{
		ID:            n.ID.String(),
		Recipient:     n.Recipient,
		Channel:       string(n.Channel),
		Content:       n.Content,
		Priority:      string(priority),
		CorrelationID: n.CorrelationID,
	}
	if n.TemplateID != nil {
		s := n.TemplateID.String()
		env.TemplateID = &s
	}
	if len(n.Payload) > 0 {
		env.Payload = json.RawMessage(n.Payload)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	pub := amqp091.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp091.Persistent,
		Priority:     domain.QueuePriority(priority),
		Timestamp:    time.Now().UTC(),
		Body:         body,
		Headers:      amqp091.Table{},
	}
	if n.CorrelationID != nil && *n.CorrelationID != "" {
		pub.Headers["correlation_id"] = *n.CorrelationID
	}
	return ch.PublishWithContext(ctx, ExchangeTopic, routingKey, false, false, pub)
}

func (c *Client) Consume(ctx context.Context, ch domain.Channel, handler func(context.Context, amqp091.Delivery) error) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || conn.IsClosed() {
		return fmt.Errorf("amqp consume: not connected")
	}
	q, ok := queueByChannel[ch]
	if !ok {
		return fmt.Errorf("unknown channel %q", ch)
	}
	subCh, err := conn.Channel()
	if err != nil {
		return err
	}
	defer subCh.Close()
	if err := subCh.Qos(10, 0, false); err != nil {
		return err
	}
	tag := fmt.Sprintf("worker-%s", ch)
	msgs, err := subCh.Consume(q, tag, false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_ = subCh.Cancel(tag, false)
			return ctx.Err()
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("consumer channel closed")
			}
			if err := handler(ctx, d); err != nil {
				_ = d.Nack(false, false)
				continue
			}
			if err := d.Ack(false); err != nil {
				return err
			}
		}
	}
}
