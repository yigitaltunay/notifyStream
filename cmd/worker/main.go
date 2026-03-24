package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/google/uuid"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/config"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/delivery"
	"github.com/yaltunay/notifystream/internal/domain"
	"github.com/yaltunay/notifystream/internal/metrics"
	"github.com/yaltunay/notifystream/internal/ratelimit"
	"github.com/yaltunay/notifystream/internal/tracing"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const maxWebhookRetries = 5

// pauseBeforeRequeue avoids a hot loop when Nack(requeue=true) redelivers immediately (rate limit, Redis, republish errors).
func pauseBeforeRequeue(ctx context.Context, d amqp091.Delivery) {
	rc := amqp.RetryCount(d.Headers)
	ms := 100 + rc*80
	if ms > 2500 {
		ms = 2500
	}
	j := time.Duration(time.Now().UnixNano() % int64(60*time.Millisecond))
	wait := time.Duration(ms)*time.Millisecond + j
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

func revertSendingAndRequeue(ctx context.Context, store *db.Store, id uuid.UUID, cause error, d amqp091.Delivery) error {
	pauseBeforeRequeue(ctx, d)
	rctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := store.ReleaseSendingToQueued(rctx, id); err != nil {
		slog.Error("release sending to queued", "error", err, "notification_id", id)
	}
	slog.Warn("requeue after release sending", "notification_id", id, "cause", cause.Error(), "x_retry_count", amqp.RetryCount(d.Headers))
	return fmt.Errorf("%w: %v", amqp.ErrRequeueDelivery, cause)
}

func markDeliveredWithRetry(ctx context.Context, store *db.Store, id uuid.UUID, mid *string) error {
	const maxAttempts = 30
	mdCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if err := mdCtx.Err(); err != nil {
			return fmt.Errorf("mark delivered: %w (last=%v)", err, lastErr)
		}
		if err := store.MarkDelivered(mdCtx, id, mid); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i+1 == maxAttempts {
			break
		}
		shift := i
		if shift > 6 {
			shift = 6
		}
		wait := time.Duration(50*(1<<shift)) * time.Millisecond
		if wait > 2*time.Second {
			wait = 2 * time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("mark delivered after %d attempts: %w", maxAttempts, lastErr)
}

func stuckSendingRecovery() time.Duration {
	const def = 120
	s := strings.TrimSpace(os.Getenv("STUCK_SENDING_RECOVERY_SECONDS"))
	if s == "" {
		return time.Duration(def) * time.Second
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return time.Duration(def) * time.Second
	}
	if v == 0 {
		return 0
	}
	return time.Duration(v) * time.Second
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(true)
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTrace, err := tracing.Setup(ctx, "notifystream-worker")
	if err != nil {
		slog.Error("tracing", "error", err)
		os.Exit(1)
	}
	defer func() { _ = shutdownTrace(context.Background()) }()

	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("metrics server", "error", err)
			}
		}()
		slog.Info("metrics listening", "addr", cfg.MetricsAddr)
	}

	pool, err := db.OpenPool(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := db.NewStore(pool)
	bus := amqp.NewClient(cfg.AMQPURL)
	if err := bus.Connect(ctx); err != nil {
		slog.Error("amqp", "error", err)
		os.Exit(1)
	}
	defer func() { _ = bus.Close() }()

	wh := delivery.NewWebhook(cfg.WebhookURL, otelhttp.NewTransport(http.DefaultTransport))

	limWaiter, stopLim, err := ratelimit.NewChannelWaiter(cfg.RedisURL, 100, 100)
	if err != nil {
		slog.Error("rate limiter", "error", err)
		os.Exit(1)
	}
	defer stopLim()
	if cfg.RedisURL != "" {
		slog.Info("delivery rate limit", "backend", "redis")
	} else {
		slog.Info("delivery rate limit", "backend", "memory_per_worker")
	}

	stuckRec := stuckSendingRecovery()
	if stuckRec > 0 {
		slog.Info("stale sending recovery", "after", stuckRec.String())
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, ch := range []domain.Channel{domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush} {
		ch := ch
		g.Go(func() error {
			slog.Info("consumer started", "channel", ch)
			return bus.Consume(ctx, ch, func(c context.Context, d amqp091.Delivery) error {
				return handleDelivery(c, store, bus, wh, limWaiter, ch, d, stuckRec)
			})
		})
	}
	if err := g.Wait(); err != nil && ctx.Err() == nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
	slog.Info("worker shutting down")
}

func enrichEnvelopeFromDelivery(env *amqp.Envelope, d amqp091.Delivery) {
	if env.CorrelationID != nil && strings.TrimSpace(*env.CorrelationID) != "" {
		return
	}
	if d.Headers == nil {
		return
	}
	v, ok := d.Headers["correlation_id"]
	if !ok {
		return
	}
	var s string
	switch t := v.(type) {
	case string:
		s = strings.TrimSpace(t)
	case []byte:
		s = strings.TrimSpace(string(t))
	default:
		return
	}
	if s == "" {
		return
	}
	env.CorrelationID = &s
}

func handleDelivery(ctx context.Context, store *db.Store, bus *amqp.Client, wh *delivery.Webhook, lim ratelimit.ChannelWaiter, ch domain.Channel, d amqp091.Delivery, stuckRecover time.Duration) error {
	start := time.Now()
	ctx = amqp.ExtractTraceCtx(ctx, d.Headers)
	var env amqp.Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		slog.Warn("bad message body", "error", err)
		return err
	}
	enrichEnvelopeFromDelivery(&env, d)
	log := slog.Default()
	if env.CorrelationID != nil && *env.CorrelationID != "" {
		log = log.With("correlation_id", *env.CorrelationID)
	}

	id, err := uuid.Parse(env.ID)
	if err != nil {
		slog.Warn("bad message id", "error", err)
		return err
	}
	log = log.With("notification_id", id.String())
	log.Info("delivery received",
		"routing_key", d.RoutingKey,
		"consumer_channel", string(ch),
		"delivery_tag", d.DeliveryTag,
		"retry_count", amqp.RetryCount(d.Headers),
		"message", string(d.Body),
	)

	n, err := store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	chLabel := string(ch)

	if n.Status == domain.StatusCancelled || n.Status == domain.StatusDelivered {
		return nil
	}

	if n.Status != domain.StatusPending && n.Status != domain.StatusQueued && n.Status != domain.StatusSending {
		log.Info("skip consume; unexpected status", "status", n.Status)
		return nil
	}

	outEnv, berr := buildOutboundEnvelope(ctx, store, n, env)
	if berr != nil {
		if delivery.IsPermanent(berr) {
			metrics.NotificationsFailed.WithLabelValues(chLabel, "validation").Inc()
			_ = store.MarkFailed(ctx, id)
			emitStatus(ctx, bus, n, "failed")
			log.Warn("envelope validation", "error", berr)
			return nil
		}
		return berr
	}

	rlWait := time.Now()
	if err := lim.Wait(ctx, ch); err != nil {
		pauseBeforeRequeue(ctx, d)
		slog.Warn("requeue after rate limit wait", "notification_id", id, "error", err, "x_retry_count", amqp.RetryCount(d.Headers))
		return fmt.Errorf("%w: %v", amqp.ErrRequeueDelivery, err)
	}
	metrics.RateLimitWait.Observe(time.Since(rlWait).Seconds())

	if err := store.MarkSending(ctx, id); err != nil {
		n2, _ := store.GetByID(ctx, id)
		if n2.Status == domain.StatusCancelled || n2.Status == domain.StatusDelivered {
			return nil
		}
		log.Info("skip send; state conflict", "status", n2.Status)
		return nil
	}

	msgID, postErr := wh.Post(ctx, outEnv)
	if postErr == nil {
		metrics.DeliveryLatency.WithLabelValues(chLabel).Observe(time.Since(start).Seconds())
		metrics.NotificationsSent.WithLabelValues(chLabel).Inc()
		var mid *string
		if msgID != "" {
			mid = &msgID
		}
		if err := markDeliveredWithRetry(ctx, store, id, mid); err != nil {
			slog.Error("mark delivered failed after webhook 2xx (row left sending; fix DB or replay)", "error", err, "notification_id", id)
			return err
		}
		emitStatus(ctx, bus, n, "delivered")
		log.Info("delivered")
		return nil
	}

	if delivery.IsPermanent(postErr) {
		metrics.DeliveryLatency.WithLabelValues(chLabel).Observe(time.Since(start).Seconds())
		metrics.NotificationsFailed.WithLabelValues(chLabel, "permanent").Inc()
		_ = store.MarkFailed(ctx, id)
		emitStatus(ctx, bus, n, "failed")
		log.Warn("delivery permanent failure", "error", postErr)
		return nil
	}

	if !delivery.IsTransient(postErr) {
		metrics.NotificationsFailed.WithLabelValues(chLabel, "unknown").Inc()
		_ = store.MarkFailed(ctx, id)
		emitStatus(ctx, bus, n, "failed")
		log.Error("delivery unexpected error", "error", postErr)
		return nil
	}

	rc := amqp.RetryCount(d.Headers)
	if rc >= maxWebhookRetries {
		metrics.DeliveryLatency.WithLabelValues(chLabel).Observe(time.Since(start).Seconds())
		metrics.NotificationsFailed.WithLabelValues(chLabel, "retries_exhausted").Inc()
		_ = store.MarkFailed(ctx, id)
		emitStatus(ctx, bus, n, "failed")
		log.Warn("delivery retries exhausted", "error", postErr, "x_retry_count", rc)
		return nil
	}

	backoff := retryBackoff(rc)
	select {
	case <-ctx.Done():
		return revertSendingAndRequeue(ctx, store, id, ctx.Err(), d)
	case <-time.After(backoff):
	}

	h := cloneHeaders(d.Headers)
	if err := bus.RepublishDelivery(ctx, d.RoutingKey, d.Body, d.Priority, h); err != nil {
		return revertSendingAndRequeue(ctx, store, id, err, d)
	}
	log.Info("delivery transient; republished", "detail", postErr.Error(), "next_retry", rc+1)
	return nil
}

func buildOutboundEnvelope(ctx context.Context, store *db.Store, n domain.Notification, env amqp.Envelope) (amqp.Envelope, error) {
	out := env
	out.TemplateID = nil
	out.Payload = nil
	if n.Content != nil && strings.TrimSpace(*n.Content) != "" {
		c := strings.TrimSpace(*n.Content)
		out.Content = &c
		return out, nil
	}
	if n.TemplateID == nil {
		return amqp.Envelope{}, &delivery.PermanentError{Detail: "missing content"}
	}
	tpl, err := store.GetTemplate(ctx, *n.TemplateID)
	if err != nil {
		return amqp.Envelope{}, err
	}
	if tpl.Channel != n.Channel {
		return amqp.Envelope{}, &delivery.PermanentError{Detail: "template channel mismatch"}
	}
	text, err := domain.RenderTemplateBody(tpl.Body, n.Payload)
	if err != nil {
		return amqp.Envelope{}, &delivery.PermanentError{Detail: err.Error()}
	}
	out.Content = &text
	return out, nil
}

func emitStatus(ctx context.Context, bus *amqp.Client, n domain.Notification, status string) {
	ev := map[string]any{
		"notification_id": n.ID.String(),
		"status":          status,
	}
	if n.BatchID != nil {
		ev["batch_id"] = n.BatchID.String()
	}
	if n.CorrelationID != nil && *n.CorrelationID != "" {
		ev["correlation_id"] = *n.CorrelationID
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := bus.PublishStatus(ctx, b); err != nil {
		slog.Warn("publish status", "error", err)
	}
}

func cloneHeaders(h amqp091.Table) amqp091.Table {
	if h == nil {
		return amqp091.Table{}
	}
	out := amqp091.Table{}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func retryBackoff(retryCount int) time.Duration {
	const base = 250 * time.Millisecond
	const max = 30 * time.Second
	d := base
	for i := 0; i < retryCount && d < max; i++ {
		next := d * 2
		if next > max {
			d = max
			break
		}
		d = next
	}
	jitter := time.Duration(time.Now().UnixNano() % int64(50*time.Millisecond))
	return d + jitter
}
