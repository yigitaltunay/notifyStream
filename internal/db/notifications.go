package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yaltunay/notifystream/internal/domain"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) CreateBatch(ctx context.Context, tx pgx.Tx, metadata []byte, count int) (uuid.UUID, error) {
	var id uuid.UUID
	var meta any
	if len(metadata) > 0 {
		meta = metadata
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO notification_batches (metadata, notification_count)
		VALUES ($1::jsonb, $2)
		RETURNING id
	`, meta, count).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Store) InsertNotification(ctx context.Context, tx pgx.Tx, batchID *uuid.UUID, item domain.CreateItem, correlationID *string) (domain.Notification, bool, error) {
	priority := item.Priority
	if priority == "" {
		priority = domain.PriorityNormal
	}
	var bid any
	if batchID != nil {
		bid = *batchID
	}
	var tpl any
	if item.TemplateID != nil {
		tpl = *item.TemplateID
	}
	var idem any
	if item.IdempotencyKey != nil && strings.TrimSpace(*item.IdempotencyKey) != "" {
		idem = strings.TrimSpace(*item.IdempotencyKey)
	}
	var content any
	if item.Content != nil {
		content = strings.TrimSpace(*item.Content)
	}
	var payload any
	if len(item.Payload) > 0 {
		payload = item.Payload
	}
	var sched any
	if item.ScheduledAt != nil {
		sched = *item.ScheduledAt
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO notifications (
			batch_id, recipient, channel, content, template_id, payload,
			priority, status, idempotency_key, scheduled_at, correlation_id
		) VALUES (
			$1, $2, $3::notification_channel, $4, $5, $6::jsonb,
			$7::notification_priority, 'pending', $8, $9, $10
		)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL
		DO NOTHING
		RETURNING
			id, batch_id, recipient, channel::text, content, template_id, payload,
			priority::text, status::text, idempotency_key, provider_message_id,
			scheduled_at, correlation_id, created_at, updated_at
	`, bid, strings.TrimSpace(item.Recipient), string(item.Channel), content, tpl, payload,
		string(priority), idem, sched, correlationID,
	)
	n, err := scanNotificationRow(row)
	if err == nil {
		return n, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, false, err
	}
	if idem == nil {
		return domain.Notification{}, false, fmt.Errorf("insert returned no row without idempotency key")
	}
	return s.notificationByIdempotencyKeyTx(ctx, tx, idem.(string))
}

func (s *Store) notificationByIdempotencyKeyTx(ctx context.Context, tx pgx.Tx, key string) (domain.Notification, bool, error) {
	n, err := s.scanNotification(tx.QueryRow(ctx, `
		SELECT
			id, batch_id, recipient, channel::text, content, template_id, payload,
			priority::text, status::text, idempotency_key, provider_message_id,
			scheduled_at, correlation_id, created_at, updated_at
		FROM notifications
		WHERE idempotency_key = $1
	`, key))
	if err != nil {
		return domain.Notification{}, false, err
	}
	return n, false, nil
}

func (s *Store) scanNotification(row pgx.Row) (domain.Notification, error) {
	return scanNotificationRow(row)
}

func (s *Store) MarkSending(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = 'sending', updated_at = now()
		WHERE id = $1 AND status = 'queued'
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("notification %s not queued", id)
	}
	return nil
}

func (s *Store) MarkDelivered(ctx context.Context, id uuid.UUID, providerMessageID *string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = 'delivered', provider_message_id = COALESCE($2, provider_message_id), updated_at = now()
		WHERE id = $1 AND status = 'sending'
	`, id, providerMessageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("notification %s not sending", id)
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = 'failed', updated_at = now()
		WHERE id = $1 AND status IN ('queued', 'sending')
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("notification %s cannot be marked failed from current state", id)
	}
	return nil
}

func (s *Store) MarkQueued(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE notifications
		SET status = 'queued', updated_at = now()
		WHERE id = $1 AND status = 'pending'
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("notification %s not pending", id)
	}
	return nil
}

func (s *Store) EnqueueOutbox(ctx context.Context, tx pgx.Tx, notificationID uuid.UUID, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (notification_id, payload)
		VALUES ($1, $2::jsonb)
	`, notificationID, b)
	return err
}

func (s *Store) Cancel(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	return s.scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications
		SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'queued')
		RETURNING
			id, batch_id, recipient, channel::text, content, template_id, payload,
			priority::text, status::text, idempotency_key, provider_message_id,
			scheduled_at, correlation_id, created_at, updated_at
	`, id))
}

func (s *Store) GetByID(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	return s.scanNotification(s.pool.QueryRow(ctx, `
		SELECT
			id, batch_id, recipient, channel::text, content, template_id, payload,
			priority::text, status::text, idempotency_key, provider_message_id,
			scheduled_at, correlation_id, created_at, updated_at
		FROM notifications
		WHERE id = $1
	`, id))
}

func (s *Store) ListByBatchID(ctx context.Context, batchID uuid.UUID) ([]domain.Notification, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			id, batch_id, recipient, channel::text, content, template_id, payload,
			priority::text, status::text, idempotency_key, provider_message_id,
			scheduled_at, correlation_id, created_at, updated_at
		FROM notifications
		WHERE batch_id = $1
		ORDER BY created_at ASC, id ASC
	`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Notification
	for rows.Next() {
		n, err := scanNotificationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type ListParams struct {
	Status    *domain.Status
	Channel   *domain.Channel
	From      *time.Time
	To        *time.Time
	CursorAt  *time.Time
	CursorID  *uuid.UUID
	Limit     int
}

func (s *Store) List(ctx context.Context, p ListParams) ([]domain.Notification, error) {
	if p.Limit <= 0 {
		p.Limit = 20
	}
	if p.Limit > 100 {
		p.Limit = 100
	}
	var st any
	if p.Status != nil {
		st = string(*p.Status)
	}
	var ch any
	if p.Channel != nil {
		ch = string(*p.Channel)
	}
	var from, to any
	if p.From != nil {
		from = *p.From
	}
	if p.To != nil {
		to = *p.To
	}
	var cAt any
	var cID any
	if p.CursorAt != nil && p.CursorID != nil {
		cAt = *p.CursorAt
		cID = *p.CursorID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT
			id, batch_id, recipient, channel::text, content, template_id, payload,
			priority::text, status::text, idempotency_key, provider_message_id,
			scheduled_at, correlation_id, created_at, updated_at
		FROM notifications
		WHERE ($1::text IS NULL OR status::text = $1)
		  AND ($2::text IS NULL OR channel::text = $2)
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		  AND ($4::timestamptz IS NULL OR created_at <= $4)
		  AND (
		    $5::timestamptz IS NULL
		    OR created_at < $5
		    OR (created_at = $5 AND id < $6::uuid)
		  )
		ORDER BY created_at DESC, id DESC
		LIMIT $7
	`, st, ch, from, to, cAt, cID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Notification
	for rows.Next() {
		n, err := scanNotificationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func scanNotificationRow(row pgx.Row) (domain.Notification, error) {
	var n domain.Notification
	var batchID *uuid.UUID
	var content *string
	var tplID *uuid.UUID
	var payload []byte
	var idem *string
	var prov *string
	var corr *string
	err := row.Scan(
		&n.ID, &batchID, &n.Recipient, &n.Channel, &content, &tplID, &payload,
		&n.Priority, &n.Status, &idem, &prov,
		&n.ScheduledAt, &corr, &n.CreatedAt, &n.UpdatedAt,
	)
	if err != nil {
		return domain.Notification{}, err
	}
	n.BatchID = batchID
	n.Content = content
	n.TemplateID = tplID
	n.Payload = payload
	n.IdempotencyKey = idem
	n.ProviderMessageID = prov
	n.CorrelationID = corr
	return n, nil
}
