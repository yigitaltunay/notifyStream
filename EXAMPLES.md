# NotifyStream — HTTP API examples (detailed cookbook)

This document is the **practical reference** for JSON request bodies. Swagger UI at `/swagger/*` sometimes renders `json.RawMessage` fields (`batch_metadata`, `payload`) with misleading schemas (e.g. arrays of integers). **Trust the examples below** for the real contract.

**Conventions**

- All `POST`/`PUT`-style calls use `Content-Type: application/json`.
- Replace placeholders such as `NOTIFICATION-UUID` with real UUIDs from API responses.
- The external delivery target is configured as `WEBHOOK_URL` on workers (e.g. [webhook.site](https://webhook.site)).

---

## Table of contents

1. [Environment and headers](#environment-and-headers)
2. [Create notifications — response shape](#create-notifications--response-shape)
3. [Field reference](#field-reference)
4. [Minimal SMS (single item)](#minimal-sms-single-item)
5. [Email — subject and body encoding](#email--subject-and-body-encoding)
6. [Push — title and body encoding](#push--title-and-body-encoding)
7. [Batch create with `batch_metadata`](#batch-create-with-batch_metadata)
8. [Idempotency and correlation IDs](#idempotency-and-correlation-ids)
9. [Scheduled delivery (`scheduled_at`)](#scheduled-delivery-scheduled_at)
10. [Templates — create then notify](#templates--create-then-notify)
11. [Get one notification](#get-one-notification)
12. [List notifications in a batch](#list-notifications-in-a-batch)
13. [List with filters and cursor pagination](#list-with-filters-and-cursor-pagination)
14. [Cancel a notification](#cancel-a-notification)
15. [Validation errors (400)](#validation-errors-400)
16. [Health, readiness, and Prometheus metrics](#health-readiness-and-prometheus-metrics)
17. [WebSocket status stream](#websocket-status-stream)
18. [Swagger UI caveat](#swagger-ui-caveat)
19. [Quick reference table](#quick-reference-table)

---

## Environment and headers

```bash
export BASE=http://localhost:8080
```

| Header | When | Purpose |
|--------|------|---------|
| `X-Request-ID` | Optional on any request | Correlation ID; echoed on the response; stored on the notification and propagated to workers / logs. If omitted, the server may generate one (Chi middleware). |

---

## Create notifications — response shape

**Endpoint:** `POST /v1/notifications`  
**Limits:** 1–1000 items in `notifications`.

Successful response is **201** with a body like:

```json
{
  "batch_id": "550e8400-e29b-41d4-a716-446655440000",
  "notifications": [
    {
      "id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
      "batch_id": "550e8400-e29b-41d4-a716-446655440000",
      "recipient": "+15551234567",
      "channel": "sms",
      "content": "Hello",
      "priority": "normal",
      "status": "queued",
      "inserted": true,
      "correlation_id": "req-abc",
      "created_at": "2025-03-24T12:00:00Z",
      "updated_at": "2025-03-24T12:00:00Z"
    }
  ],
  "idempotent_hits": 0
}
```

**Notes**

- **`batch_id`** (top-level and per row): Present when **more than one** notification is created in the same request. For a **single-item** array, the API does **not** create a batch header row, so `batch_id` is typically `null`/omitted.
- **`inserted`**: `true` if this call inserted a new row; `false` if the row was returned due to **`idempotency_key`** conflict (no duplicate enqueue).
- **`status`**: Immediately after create, successful publishes show `queued`; items scheduled for the future stay `pending` until the scheduler process publishes them. If AMQP publish fails, the row may stay `pending` and an **outbox** row is written for retry.

---

## Field reference

| JSON field | Type | Required | Description |
|------------|------|----------|-------------|
| `notifications` | array | Yes | 1–1000 items. |
| `batch_metadata` | object | No | Arbitrary JSON **object** for your own auditing (campaign id, source system, etc.). Ignored when only one notification is sent (no batch row). |
| `recipient` | string | Yes | Destination (E.164-ish phone for SMS, email address, or push token / user id — whatever your downstream provider expects). |
| `channel` | string | Yes | One of: `sms`, `email`, `push`. |
| `priority` | string | No | `high`, `normal`, `low`. Empty defaults to `normal` server-side. |
| `content` | string | Conditional | Plain text body. Required unless you use `template_id` + `payload`. |
| `template_id` | UUID string | Conditional | References a row in `templates`. Must match notification `channel`. Requires `payload`. |
| `payload` | object | Conditional | JSON object whose keys match `{{key}}` placeholders in the template body. |
| `idempotency_key` | string | No | Unique per logical send; duplicates return the existing notification and do not enqueue again. |
| `scheduled_at` | string (RFC3339) | No | If in the **future**, the message is not published until due (scheduler). If absent or in the past/now, publish is attempted immediately after commit. |

**RabbitMQ mapping:** `priority` affects routing key `notify.{channel}.{priority}` and AMQP message priority (high > normal > low).

---

## Minimal SMS (single item)

Use when you send one SMS with inline text. No batch is created.

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "notifications": [
      {
        "recipient": "+15551234567",
        "channel": "sms",
        "content": "Your code is 482910",
        "priority": "normal"
      }
    ]
  }'
```

**Validation:** SMS `content` length is capped (see `internal/domain/notification.go`, constant `maxSMSLen`).

---

## Email — subject and body encoding

The API does **not** use separate `subject` / `body` fields. Encode both in **`content`**:

- **First line** (up to first `\n`) → subject  
- **Remainder** → body  

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "notifications": [
      {
        "recipient": "user@example.com",
        "channel": "email",
        "content": "Weekly digest\nYou have 3 new messages this week.",
        "priority": "low"
      }
    ]
  }'
```

**Validation:** Subject and body have separate max lengths (`maxEmailSubject`, `maxEmailBody` in code).

---

## Push — title and body encoding

Same pattern as email for encoding:

- **First line** → notification title  
- **After first `\n`** → body (optional)  

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "notifications": [
      {
        "recipient": "device-token-or-user-id",
        "channel": "push",
        "content": "Flash sale\n50% off ends tonight",
        "priority": "high"
      }
    ]
  }'
```

---

## Batch create with `batch_metadata`

When **`notifications` has 2+ items**, the API creates a **`notification_batches`** row and sets **`batch_id`** on each inserted notification.

`batch_metadata` is a **JSON object** (not a string). You can store campaign identifiers, job names, or trace links.

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "batch_metadata": {
      "campaign_id": "flash-2025-03",
      "source": "crm-export",
      "requested_by": "batch-job-7"
    },
    "notifications": [
      {
        "recipient": "+15551111111",
        "channel": "sms",
        "content": "You are next in line.",
        "priority": "high"
      },
      {
        "recipient": "+15552222222",
        "channel": "sms",
        "content": "You are next in line.",
        "priority": "high"
      }
    ]
  }'
```

**Follow-up:** list all items with `GET /v1/batches/{batchId}/notifications`.

---

## Idempotency and correlation IDs

**Idempotency** prevents duplicate side effects when clients retry after timeouts or `5xx`.

- Send a stable **`idempotency_key`** per logical notification (e.g. `order-12345-sms-receipt`).
- The second request with the same key returns the **same** notification; **`inserted: false`**; **`idempotent_hits`** in the top-level response increments.

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -H 'X-Request-ID: checkout-8842' \
  -d '{
    "notifications": [
      {
        "recipient": "+15559998877",
        "channel": "sms",
        "content": "Your order was received.",
        "priority": "normal",
        "idempotency_key": "order-8842-sms-confirmation"
      }
    ]
  }'
```

**Correlation:** `X-Request-ID` is stored as `correlation_id` on the row and helps tie HTTP logs, AMQP headers, and worker logs together.

---

## Scheduled delivery (`scheduled_at`)

Use an **RFC3339** timestamp in the future. The row stays **`pending`** until the **`scheduler`** process (`cmd/scheduler`) publishes it to RabbitMQ, then status becomes **`queued`**.

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "notifications": [
      {
        "recipient": "+15551234567",
        "channel": "sms",
        "content": "Reminder: meeting tomorrow at 10:00.",
        "priority": "normal",
        "scheduled_at": "2030-12-01T09:00:00Z"
      }
    ]
  }'
```

**Mixed batch:** You can combine immediate and scheduled items in one request; each item is evaluated independently.

---

## Templates — create then notify

**Step 1 — create a template** (`POST /v1/templates`)

Placeholders use `{{variable_name}}`. Names must match keys in `payload` on the notification.

```bash
curl -sS -X POST "$BASE/v1/templates" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "otp_sms_en",
    "body": "Hi {{name}}, your verification code is {{code}}. It expires in 10 minutes.",
    "channel": "sms",
    "variables_schema": {
      "type": "object",
      "required": ["name", "code"]
    }
  }'
```

- **`name`**: Unique; duplicate returns **409** `template name already exists`.
- **`variables_schema`**: Optional JSON metadata for documentation/validation in your tooling; the worker still substitutes using `{{key}}` and `payload` keys.

**Step 2 — send using `template_id`**

Use the UUID from the create response. **`payload`** must be a **JSON object** (not a string).

```bash
curl -sS -X POST "$BASE/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "notifications": [
      {
        "recipient": "+15551234567",
        "channel": "sms",
        "template_id": "PASTE-TEMPLATE-UUID-HERE",
        "payload": {
          "name": "Ada",
          "code": "739201"
        },
        "priority": "high"
      }
    ]
  }'
```

**Rules**

- Do **not** send `content` for template-based items; validation requires `template_id` + non-empty `payload`.
- The worker resolves the template and sends plain `content` to `WEBHOOK_URL`.

---

## Get one notification

```bash
curl -sS "$BASE/v1/notifications/6ba7b810-9dad-11d1-80b4-00c04fd430c8"
```

**404** if the UUID does not exist.

---

## List notifications in a batch

Returns all notifications for a batch, ordered by creation time.

```bash
curl -sS "$BASE/v1/batches/550e8400-e29b-41d4-a716-446655440000/notifications"
```

---

## List with filters and cursor pagination

**Endpoint:** `GET /v1/notifications`

| Query | Description |
|-------|-------------|
| `status` | `pending`, `queued`, `sending`, `delivered`, `failed`, `cancelled` |
| `channel` | `sms`, `email`, `push` |
| `from`, `to` | RFC3339 timestamps; filter `created_at` (inclusive window as implemented in SQL) |
| `limit` | Page size, default 20, max 100 |
| `cursor` | Opaque cursor from previous response’s `next_cursor` |

```bash
curl -sS "$BASE/v1/notifications?status=delivered&channel=sms&limit=10"

curl -sS "$BASE/v1/notifications?from=2025-01-01T00:00:00Z&to=2025-12-31T23:59:59Z&limit=20"

curl -sS "$BASE/v1/notifications?limit=20&cursor=PASTE_NEXT_CURSOR_FROM_PREVIOUS_RESPONSE"
```

**Pagination:** If `next_cursor` is non-empty, more rows exist. Pass it as `cursor` on the next call.

---

## Cancel a notification

**Endpoint:** `POST /v1/notifications/{id}/cancel`  
**Allowed states:** `pending`, `queued`.  
**Result:** Row becomes `cancelled`.

```bash
curl -sS -X POST "$BASE/v1/notifications/6ba7b810-9dad-11d1-80b4-00c04fd430c8/cancel"
```

**409** if the notification cannot be cancelled (e.g. already `sending` / `delivered` / `failed`).

---

## Validation errors (400)

Batch validation returns **400** with per-index errors:

```json
{
  "errors": [
    { "index": 0, "message": "invalid channel" },
    { "index": 2, "message": "sms content exceeds 1600 characters" }
  ]
}
```

Single-field failures use:

```json
{ "error": "invalid json body" }
```

---

## Health, readiness, and Prometheus metrics

### HTTP endpoints

| Endpoint | Purpose |
|----------|---------|
| `GET /healthz` | Liveness (process up). |
| `GET /readyz` | Readiness: PostgreSQL ping + AMQP connectivity. |
| `GET /metrics` | Prometheus **text exposition** on the **API** process (`Content-Type`: OpenMetrics/Prometheus text, not JSON). Documented in Swagger under tag **observability**. |

```bash
curl -sS "$BASE/healthz"
curl -sS "$BASE/readyz"
curl -sS "$BASE/metrics" | head -80
```

### Where to scrape for delivery stats

NotifyStream increments **delivery-related** counters and histograms in the **worker** only (`cmd/worker`). The API imports the same metric definitions so `/metrics` on the API exposes the same **metric names**, but those series usually stay at **zero** on the API process.

| Process | URL (typical) | What you get |
|---------|----------------|--------------|
| **Worker** | `http://localhost:9091/metrics` when `METRICS_ADDR=:9091` | **Use this** for `notifystream_*` delivery, failures, latency, rate-limit wait. |
| **API** | `http://localhost:8080/metrics` | Standard **Go** (`go_*`) and **process** (`process_*`) metrics from the Prometheus client, plus `notifystream_*` registered at zero unless you add API-side instrumentation later. |

**Docker Compose:** map worker port `9091:9091` and set `METRICS_ADDR=:9091` in `.env` (see `.env.example`).

### Application metrics (`notifystream_*`)

Defined in `internal/metrics/metrics.go`. All use the **default** Prometheus registry.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `notifystream_notifications_sent_total` | Counter | `channel` (`sms`, `email`, `push`) | Webhook returned **2xx**; delivery accepted. |
| `notifystream_notifications_failed_total` | Counter | `channel`, `outcome` | Terminal failure or exhausted retries. Typical `outcome` values: `validation` (template/body error before send), `permanent` (4xx from webhook), `unknown`, `retries_exhausted`. |
| `notifystream_delivery_latency_seconds` | Histogram | `channel` | Seconds from worker **pickup** (after rate limiter) until webhook response completes. |
| `notifystream_rate_limit_wait_seconds` | Histogram | (none) | Seconds blocked waiting for the per-channel **100 req/s** token bucket before calling the webhook. |

**Example — filter worker scrape with `grep`:**

```bash
curl -sS "http://localhost:9091/metrics" | egrep '^notifystream_'
```

**PromQL-style questions (Grafana / Prometheus UI):**

- Delivery rate per channel: `rate(notifystream_notifications_sent_total[5m])`
- Failure rate by outcome: `rate(notifystream_notifications_failed_total[5m])`
- p95 delivery latency (SMS): `histogram_quantile(0.95, sum(rate(notifystream_delivery_latency_seconds_bucket{channel="sms"}[5m])) by (le))`

### Not implemented (assignment gap)

**RabbitMQ queue depth** (e.g. a gauge from the management HTTP API) is **not** emitted by this codebase. If you need it, add a collector that polls `GET /api/queues` on the broker or use RabbitMQ’s Prometheus plugin.

### Other lines on `/metrics`

You will also see standard runtime metrics (names vary by Go/client version), for example:

- `go_goroutines`, `go_memstats_*`
- `process_cpu_seconds_total`, `process_resident_memory_bytes`

These help monitor **memory and CPU** of each process independently of notification business logic.

---

## WebSocket status stream

The API exposes **`GET /v1/ws`** (WebSocket upgrade). Subscribe with at least one query parameter:

- `notification_id={uuid}`
- `batch_id={uuid}` (optional second filter)

Example using [websocat](https://github.com/vi/websocat):

```bash
websocat "ws://localhost:8080/v1/ws?notification_id=6ba7b810-9dad-11d1-80b4-00c04fd430c8"
```

```bash
websocat "ws://localhost:8080/v1/ws?notification_id=6ba7b810-9dad-11d1-80b4-00c04fd430c8&batch_id=550e8400-e29b-41d4-a716-446655440000"
```

The server pushes **text frames** containing JSON such as:

```json
{
  "notification_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
  "status": "delivered",
  "batch_id": "550e8400-e29b-41d4-a716-446655440000",
  "correlation_id": "checkout-8842"
}
```

**Scaling note:** With multiple API replicas, clients usually need **sticky sessions** or a shared pub/sub layer so WebSocket subscribers receive events regardless of which instance they hit.

---

## Swagger UI caveat

Open **`http://localhost:8080/swagger/index.html`** for interactive docs generated by swag.

Because Swagger 2.0 models struggle with `json.RawMessage`, **`batch_metadata`** and **`payload`** may appear incorrectly. This **EXAMPLES.md** file and the [README](README.md) describe the real JSON shapes.

---

## Quick reference table

| Topic | Endpoint |
|-------|----------|
| Create | `POST /v1/notifications` |
| Get | `GET /v1/notifications/{id}` |
| List | `GET /v1/notifications?...` |
| Cancel | `POST /v1/notifications/{id}/cancel` |
| Batch list | `GET /v1/batches/{batchId}/notifications` |
| Create template | `POST /v1/templates` |
| WebSocket | `GET /v1/ws?notification_id=...` |
| Metrics (API) | `GET /metrics` |
| Metrics (worker) | `GET http://localhost:9091/metrics` (if `METRICS_ADDR` set) |
| Swagger | `GET /swagger/index.html` |

---

## External webhook (reference)

Workers `POST` to `WEBHOOK_URL` with a JSON body similar to:

```json
{
  "to": "+15551234567",
  "channel": "sms",
  "content": "Your message",
  "priority": "normal"
}
```

A typical mock response (**2xx**, often **202**):

```json
{
  "messageId": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
  "status": "accepted",
  "timestamp": "2025-03-24T12:00:00.000Z"
}
```

Configure your [webhook.site](https://webhook.site) (or other mock) to return **202** and the body above for realistic testing.

---

For architecture, RabbitMQ topology, scheduler/outbox, and operations, see **[README.md](README.md)**.
