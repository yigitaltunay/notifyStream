package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/google/uuid"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/config"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/delivery"
	"github.com/yaltunay/notifystream/internal/domain"
	"github.com/yaltunay/notifystream/internal/metrics"
)

const maxWebhookRetries = 5

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(true)
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	wh := delivery.NewWebhook(cfg.WebhookURL)

	limiters := map[domain.Channel]*rate.Limiter{
		domain.ChannelSMS:   rate.NewLimiter(rate.Limit(100), 100),
		domain.ChannelEmail: rate.NewLimiter(rate.Limit(100), 100),
		domain.ChannelPush:  rate.NewLimiter(rate.Limit(100), 100),
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, ch := range []domain.Channel{domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush} {
		lim := limiters[ch]
		g.Go(func() error {
			slog.Info("consumer started", "channel", ch)
			return bus.Consume(ctx, ch, func(c context.Context, d amqp091.Delivery) error {
				return handleDelivery(c, store, bus, wh, lim, ch, d)
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

func handleDelivery(ctx context.Context, store *db.Store, bus *amqp.Client, wh *delivery.Webhook, lim *rate.Limiter, ch domain.Channel, d amqp091.Delivery) error {
	start := time.Now()
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

	n, err := store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	chLabel := string(ch)

	if n.Status == domain.StatusCancelled || n.Status == domain.StatusDelivered {
		return nil
	}
	if n.Status != domain.StatusQueued && n.Status != domain.StatusSending {
		log.Info("skip consume; unexpected status", "status", n.Status)
		return nil
	}

	if err := store.MarkSending(ctx, id); err != nil {
		n2, _ := store.GetByID(ctx, id)
		if n2.Status == domain.StatusCancelled || n2.Status == domain.StatusDelivered {
			return nil
		}
		log.Info("skip send; state conflict", "status", n2.Status)
		return nil
	}

	rlWait := time.Now()
	if err := lim.Wait(ctx); err != nil {
		return err
	}
	metrics.RateLimitWait.Observe(time.Since(rlWait).Seconds())

	msgID, postErr := wh.Post(ctx, env)
	if postErr == nil {
		metrics.DeliveryLatency.WithLabelValues(chLabel).Observe(time.Since(start).Seconds())
		metrics.NotificationsSent.WithLabelValues(chLabel).Inc()
		var mid *string
		if msgID != "" {
			mid = &msgID
		}
		if err := store.MarkDelivered(ctx, id, mid); err != nil {
			return err
		}
		log.Info("delivered")
		return nil
	}

	if delivery.IsPermanent(postErr) {
		metrics.DeliveryLatency.WithLabelValues(chLabel).Observe(time.Since(start).Seconds())
		metrics.NotificationsFailed.WithLabelValues(chLabel, "permanent").Inc()
		_ = store.MarkFailed(ctx, id)
		log.Warn("delivery permanent failure", "error", postErr)
		return postErr
	}

	if !delivery.IsTransient(postErr) {
		metrics.NotificationsFailed.WithLabelValues(chLabel, "unknown").Inc()
		_ = store.MarkFailed(ctx, id)
		log.Error("delivery unexpected error", "error", postErr)
		return postErr
	}

	rc := amqp.RetryCount(d.Headers)
	if rc >= maxWebhookRetries {
		metrics.DeliveryLatency.WithLabelValues(chLabel).Observe(time.Since(start).Seconds())
		metrics.NotificationsFailed.WithLabelValues(chLabel, "retries_exhausted").Inc()
		_ = store.MarkFailed(ctx, id)
		log.Warn("delivery retries exhausted", "error", postErr, "x_retry_count", rc)
		return postErr
	}

	backoff := retryBackoff(rc)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff):
	}

	h := cloneHeaders(d.Headers)
	if err := bus.RepublishDelivery(ctx, d.RoutingKey, d.Body, d.Priority, h); err != nil {
		return err
	}
	log.Info("delivery transient; republished", "detail", postErr.Error(), "next_retry", rc+1)
	return nil
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
