// @title NotifyStream API
// @version 1.0
// @description Notifications HTTP API (PostgreSQL + RabbitMQ).
// @host localhost:8080
// @BasePath /
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/api"
	"github.com/yaltunay/notifystream/internal/config"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/migrate"
	"github.com/yaltunay/notifystream/internal/tracing"

	_ "github.com/yaltunay/notifystream/docs"
	_ "github.com/yaltunay/notifystream/internal/metrics"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(false)
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTrace, err := tracing.Setup(ctx, "notifystream-api")
	if err != nil {
		slog.Error("tracing", "error", err)
		os.Exit(1)
	}
	defer func() { _ = shutdownTrace(context.Background()) }()

	if err := migrate.Up(db.MigrateDSN(cfg.DatabaseURL)); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}

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
	h := api.NewHandler(store, bus)
	hub := api.NewWSHub()
	go func() {
		err := bus.ConsumeStatus(ctx, func(c context.Context, d amqp091.Delivery) error {
			hub.DispatchStatus(d.Body)
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("status consumer", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(h, hub),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	slog.Info("api listening", "addr", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server", "error", err)
		os.Exit(1)
	}
}
