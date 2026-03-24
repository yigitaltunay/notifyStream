package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/domain"
)

func StartOutboxRelay(ctx context.Context, store *db.Store, bus *amqp.Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			entries, err := store.ListOutboxPending(ctx, 25)
			if err != nil {
				slog.Warn("outbox list", "error", err)
				continue
			}
			for _, e := range entries {
				if err := processOutboxEntry(ctx, store, bus, e); err != nil {
					slog.Warn("outbox publish", "error", err, "outbox_id", e.ID, "notification_id", e.NotificationID)
				}
			}
		}
	}
}

func processOutboxEntry(ctx context.Context, store *db.Store, bus *amqp.Client, e db.OutboxEntry) error {
	n, err := store.GetByID(ctx, e.NotificationID)
	if err != nil {
		return err
	}
	if n.Status != domain.StatusPending {
		tx, err := store.Pool().Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.MarkOutboxPublished(ctx, tx, e.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := bus.PublishNotification(ctx, n); err != nil {
		return err
	}
	const maxAttempts = 5
	const backoff = 30 * time.Millisecond
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		tx, err := store.Pool().Begin(ctx)
		if err != nil {
			continue
		}
		if err := store.MarkQueued(ctx, tx, e.NotificationID); err != nil {
			_ = tx.Rollback(ctx)
			continue
		}
		if err := store.MarkOutboxPublished(ctx, tx, e.ID); err != nil {
			_ = tx.Rollback(ctx)
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			continue
		}
		return nil
	}
	return fmt.Errorf("outbox finalize after publish: notification_id=%s", e.NotificationID)
}

func StartScheduler(ctx context.Context, store *db.Store, bus *amqp.Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ids, err := store.ListDueScheduled(ctx, 50)
			if err != nil {
				slog.Warn("scheduler list", "error", err)
				continue
			}
			for _, id := range ids {
				if err := processScheduled(ctx, store, bus, id); err != nil {
					slog.Warn("scheduler publish", "error", err, "notification_id", id)
				}
			}
		}
	}
}

func processScheduled(ctx context.Context, store *db.Store, bus *amqp.Client, id uuid.UUID) error {
	n, err := store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if n.Status != domain.StatusPending || n.ScheduledAt == nil {
		return nil
	}
	if err := bus.PublishNotification(ctx, n); err != nil {
		return err
	}
	if err := store.MarkQueuedWithRetry(ctx, n.ID, 5, 30*time.Millisecond); err != nil {
		return err
	}
	return nil
}
