CREATE TYPE notification_channel AS ENUM ('sms', 'email', 'push');
CREATE TYPE notification_priority AS ENUM ('high', 'normal', 'low');
CREATE TYPE notification_status AS ENUM (
    'pending',
    'queued',
    'sending',
    'delivered',
    'failed',
    'cancelled'
);

CREATE TABLE templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    body TEXT NOT NULL,
    channel notification_channel NOT NULL,
    variables_schema JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE notification_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    metadata JSONB,
    notification_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id UUID REFERENCES notification_batches (id) ON DELETE SET NULL,
    recipient TEXT NOT NULL,
    channel notification_channel NOT NULL,
    content TEXT,
    template_id UUID REFERENCES templates (id) ON DELETE SET NULL,
    payload JSONB,
    priority notification_priority NOT NULL DEFAULT 'normal',
    status notification_status NOT NULL DEFAULT 'pending',
    idempotency_key TEXT,
    provider_message_id TEXT,
    scheduled_at TIMESTAMPTZ,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notifications_body_check CHECK (
        content IS NOT NULL
        OR (
            template_id IS NOT NULL
            AND payload IS NOT NULL
        )
    )
);

CREATE UNIQUE INDEX notifications_idempotency_key_uidx ON notifications (idempotency_key)
WHERE
    idempotency_key IS NOT NULL;

CREATE INDEX idx_notifications_batch ON notifications (batch_id);

CREATE INDEX idx_notifications_status_created ON notifications (status, created_at DESC);

CREATE INDEX idx_notifications_list ON notifications (created_at DESC, id);

CREATE TABLE outbox (
    id BIGSERIAL PRIMARY KEY,
    notification_id UUID NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    payload JSONB NOT NULL,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_outbox_unpublished ON outbox (published_at)
WHERE
    published_at IS NULL;
