package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type OutboxEntry struct {
	ID             int64
	NotificationID uuid.UUID
}

func (s *Store) ListOutboxPending(ctx context.Context, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, notification_id
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		if err := rows.Scan(&e.ID, &e.NotificationID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) MarkOutboxPublished(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE outbox SET published_at = now() WHERE id = $1 AND published_at IS NULL
	`, id)
	return err
}

func (s *Store) ListDueScheduled(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM notifications
		WHERE status = 'pending'
		  AND scheduled_at IS NOT NULL
		  AND scheduled_at <= now()
		ORDER BY scheduled_at ASC, id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
