package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/config"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/runner"
	"github.com/yaltunay/notifystream/internal/tracing"
)

const tickInterval = 2 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(false)
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTrace, err := tracing.Setup(ctx, "notifystream-scheduler")
	if err != nil {
		slog.Error("tracing", "error", err)
		os.Exit(1)
	}
	defer func() { _ = shutdownTrace(context.Background()) }()

	pool, err := db.OpenPool(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	bus := amqp.NewClient(cfg.AMQPURL)
	if err := bus.Connect(ctx); err != nil {
		slog.Error("amqp", "error", err)
		os.Exit(1)
	}
	defer func() { _ = bus.Close() }()

	store := db.NewStore(pool)
	go runner.StartOutboxRelay(ctx, store, bus, tickInterval)
	go runner.StartScheduler(ctx, store, bus, tickInterval)

	slog.Info("scheduler running", "tick", tickInterval.String())
	<-ctx.Done()
	slog.Info("scheduler shutting down")
}
