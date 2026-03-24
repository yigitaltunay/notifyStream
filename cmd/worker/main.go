package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/google/uuid"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/config"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/delivery"
	"github.com/yaltunay/notifystream/internal/domain"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(true)
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	defer bus.Close()

	wh := delivery.NewWebhook(cfg.WebhookURL)

	g, ctx := errgroup.WithContext(ctx)
	for _, ch := range []domain.Channel{domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush} {
		ch := ch
		g.Go(func() error {
			slog.Info("consumer started", "channel", ch)
			return bus.Consume(ctx, ch, func(c context.Context, d amqp091.Delivery) error {
				return handleDelivery(c, store, wh, d)
			})
		})
	}
	if err := g.Wait(); err != nil && ctx.Err() == nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
	slog.Info("worker shutting down")
}

func handleDelivery(ctx context.Context, store *db.Store, wh *delivery.Webhook, d amqp091.Delivery) error {
	var env amqp.Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		slog.Warn("bad message body", "error", err)
		return err
	}
	id, err := uuid.Parse(env.ID)
	if err != nil {
		slog.Warn("bad message id", "error", err)
		return err
	}
	n, err := store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if n.Status == domain.StatusCancelled || n.Status == domain.StatusDelivered {
		return nil
	}
	if err := store.MarkSending(ctx, id); err != nil {
		n2, _ := store.GetByID(ctx, id)
		if n2.Status == domain.StatusCancelled || n2.Status == domain.StatusDelivered {
			return nil
		}
		slog.Info("skip send; not queued", "id", id, "status", n2.Status)
		return nil
	}
	msgID, err := wh.Post(ctx, env)
	if err != nil {
		slog.Warn("webhook delivery failed", "id", id, "error", err)
		_ = store.MarkFailed(ctx, id)
		return err
	}
	var mid *string
	if msgID != "" {
		mid = &msgID
	}
	if err := store.MarkDelivered(ctx, id, mid); err != nil {
		return err
	}
	slog.Info("delivered", "id", id)
	return nil
}
