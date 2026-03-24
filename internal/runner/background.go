package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/domain"
)

const (
	outboxBatchPerTick  = 25
	scheduledPerTick    = 50
	outboxClaimChunk   = 1
	scheduledClaimChunk = 1
)

func StartOutboxRelay(ctx context.Context, store *db.Store, bus *amqp.Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for range outboxBatchPerTick {
				tx, err := store.Pool().Begin(ctx)
				if err != nil {
					slog.Warn("outbox begin", "error", err)
					break
				}
				entries, err := store.ClaimOutboxBatch(ctx, tx, outboxClaimChunk)
				if err != nil {
					_ = tx.Rollback(ctx)
					slog.Warn("outbox claim", "error", err)
					break
				}
				if len(entries) == 0 {
					_ = tx.Rollback(ctx)
					break
				}
				if err := processOutboxEntry(ctx, tx, store, bus, entries[0]); err != nil {
					_ = tx.Rollback(ctx)
					slog.Warn("outbox publish", "error", err, "outbox_id", entries[0].ID, "notification_id", entries[0].NotificationID)
					continue
				}
				if err := tx.Commit(ctx); err != nil {
					slog.Warn("outbox commit", "error", err, "outbox_id", entries[0].ID)
				}
			}
		}
	}
}

func processOutboxEntry(ctx context.Context, tx pgx.Tx, store *db.Store, bus *amqp.Client, e db.OutboxEntry) error {
	n, err := store.GetByID(ctx, e.NotificationID)
	if err != nil {
		return err
	}
	if n.Status != domain.StatusPending {
		return store.MarkOutboxPublished(ctx, tx, e.ID)
	}
	if err := bus.PublishNotification(ctx, n); err != nil {
		return err
	}
	if err := store.MarkQueued(ctx, tx, e.NotificationID); err != nil {
		return err
	}
	return store.MarkOutboxPublished(ctx, tx, e.ID)
}

func StartScheduler(ctx context.Context, store *db.Store, bus *amqp.Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for range scheduledPerTick {
				tx, err := store.Pool().Begin(ctx)
				if err != nil {
					slog.Warn("scheduler begin", "error", err)
					break
				}
				ids, err := store.ClaimDueScheduled(ctx, tx, scheduledClaimChunk)
				if err != nil {
					_ = tx.Rollback(ctx)
					slog.Warn("scheduler claim", "error", err)
					break
				}
				if len(ids) == 0 {
					_ = tx.Rollback(ctx)
					break
				}
				if err := processScheduled(ctx, tx, store, bus, ids[0]); err != nil {
					_ = tx.Rollback(ctx)
					slog.Warn("scheduler publish", "error", err, "notification_id", ids[0])
					continue
				}
				if err := tx.Commit(ctx); err != nil {
					slog.Warn("scheduler commit", "error", err, "notification_id", ids[0])
				}
			}
		}
	}
}

func processScheduled(ctx context.Context, tx pgx.Tx, store *db.Store, bus *amqp.Client, id uuid.UUID) error {
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
	if err := store.MarkQueued(ctx, tx, id); err != nil {
		return fmt.Errorf("mark queued after scheduled publish: %w", err)
	}
	return nil
}
