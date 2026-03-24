package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yaltunay/notifystream/internal/domain"
)

func (s *Store) CreateTemplate(ctx context.Context, name, body string, ch domain.Channel, schema []byte) (domain.Template, error) {
	var sch any
	if len(schema) > 0 {
		sch = schema
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO templates (name, body, channel, variables_schema)
		VALUES ($1, $2, $3::notification_channel, $4::jsonb)
		RETURNING id, name, body, channel::text, variables_schema, created_at, updated_at
	`, name, body, string(ch), sch)
	return scanTemplateRow(row)
}

func (s *Store) GetTemplate(ctx context.Context, id uuid.UUID) (domain.Template, error) {
	return scanTemplateRow(s.pool.QueryRow(ctx, `
		SELECT id, name, body, channel::text, variables_schema, created_at, updated_at
		FROM templates
		WHERE id = $1
	`, id))
}

func scanTemplateRow(row pgx.Row) (domain.Template, error) {
	var t domain.Template
	var schema []byte
	err := row.Scan(&t.ID, &t.Name, &t.Body, &t.Channel, &schema, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return domain.Template{}, err
	}
	t.VariablesSchema = schema
	return t, nil
}
