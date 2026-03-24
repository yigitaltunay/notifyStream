# NotifyStream

NotifyStream is an **event-driven notification platform** written in **Go 1.23**. It accepts notification requests over HTTP, persists authoritative state in **PostgreSQL**, enqueues work on **RabbitMQ**, and processes deliveries asynchronously via **worker** processes that call an external HTTP provider (for example [webhook.site](https://webhook.site)) simulating SMS, email, and push gateways.

The design targets **high throughput**, **at-least-once** messaging semantics with explicit retries, **per-channel rate limiting**, **priority-aware** queuing, **idempotent** creation, **scheduled** delivery, **templates** with variable substitution, **real-time status** fan-out over WebSocket, and **observability** (Prometheus metrics, structured logs with correlation IDs, optional OpenTelemetry traces).

**HTTP examples (detailed English cookbook, curl + JSON):** [EXAMPLES.md](EXAMPLES.md) — use when Swagger shows odd types for `batch_metadata` / `payload` (`json.RawMessage`).

---

## Table of contents

1. [Architecture](#architecture)
2. [Components](#components)
3. [Data model](#data-model)
4. [Notification lifecycle](#notification-lifecycle)
5. [RabbitMQ topology](#rabbitmq-topology)
6. [Message envelope and tracing](#message-envelope-and-tracing)
7. [API](#api)
8. [Templates](#templates)
9. [Scheduled notifications](#scheduled-notifications)
10. [Outbox pattern](#outbox-pattern)
11. [Worker delivery and retries](#worker-delivery-and-retries)
12. [Real-time status (WebSocket)](#real-time-status-websocket)
13. [Observability](#observability)
14. [Configuration](#configuration)
15. [Running with Docker Compose](#running-with-docker-compose)
16. [Local development](#local-development)
17. [OpenAPI / Swagger](#openapi--swagger)
18. [Database migrations](#database-migrations)
19. [Testing and CI](#testing-and-ci)
20. [Scaling and operations](OPERATIONS.md)

---

## Architecture

```mermaid
flowchart TB
  subgraph clients [Clients]
    HTTP[HTTP_clients]
    WS[WebSocket_clients]
  end

  subgraph api_proc [API_process]
    API[HTTP_router]
    ST[Status_consumer]
    HUB[WS_hub]
  end

  subgraph sched_proc [Scheduler_process]
    OB[Outbox_relay]
    SCH[Due_scheduler]
  end

  subgraph data [Data_and_messaging]
    DB[(PostgreSQL)]
    RMQ[(RabbitMQ)]
  end

  subgraph worker_proc [Worker_processes]
    W1[Channel_consumers]
    WH[Webhook_HTTP]
  end

  HTTP --> API
  API --> DB
  API --> RMQ
  OB --> DB
  OB --> RMQ
  SCH --> DB
  SCH --> RMQ
  RMQ --> W1
  W1 --> WH
  W1 --> DB
  W1 --> RMQ
  RMQ --> ST
  ST --> HUB
  WS --> HUB
```

**Write path (HTTP `POST /v1/notifications`)**

1. Validate payload (channel, priority, content or template + payload, size limits).
2. Begin a database transaction; insert one or more `notifications` rows (and optionally a `notification_batches` row for multi-item requests).
3. Commit the transaction.
4. For each **newly inserted** row (not an idempotency replay):
   - If `scheduled_at` is **absent or in the past** (relative to UTC): publish an AMQP message. On success, mark the row `queued`. On publish failure, insert an `outbox` row for later retry.
   - If `scheduled_at` is **in the future**: leave the row `pending`; the **`scheduler` process** (`cmd/scheduler`) will publish when due.

**Process path (workers)**

1. Consume from per-channel queues (`q.notify.sms`, `q.notify.email`, `q.notify.push`).
2. Enforce **100 messages per second per channel** using `internal/ratelimit`: with **`REDIS_URL`** set, a shared Redis token bucket (`github.com/go-redis/redis_rate`); otherwise an in-memory limiter per worker (`golang.org/x/time/rate`).
3. Resolve final text: use stored `content` or load a **template** and render `{{variable}}` placeholders from JSON `payload`.
4. `POST` the outbound JSON to `WEBHOOK_URL`.
5. Update PostgreSQL (`delivered` / `failed`), and publish a JSON event to the **`notifications.status`** fanout for WebSocket subscribers.

---

## Components

| Artifact | Role |
| -------- | ---- |
| `cmd/api` | HTTP API, runs embedded migrations on startup, AMQP publisher for creates, **status** consumer feeding the WebSocket hub. |
| `cmd/scheduler` | Dedicated process: **outbox relay** and **due-time scheduler** (poll interval 2s). Run **one replica** (or add leader election) to avoid duplicate publishes. |
| `cmd/worker` | One or more OS processes (Compose runs one) consuming RabbitMQ and performing webhook delivery. Optional dedicated `/metrics` listener when `METRICS_ADDR` is set. |
| `internal/amqp` | Declares exchanges/queues, publishes notifications and status events, consumes work and (on API) status messages. |
| `internal/db` | PostgreSQL access via `pgxpool` (notifications, batches, templates, outbox, listing, status transitions). |
| `internal/api` | Chi router, handlers, request logging with correlation ID, Swagger wiring, WebSocket hub. |
| `internal/delivery` | Webhook HTTP client with transient vs permanent error typing. |
| `internal/ratelimit` | Per-channel delivery throttle (Redis-backed or in-memory). |
| `internal/domain` | Validation, priorities, template rendering. |
| `internal/runner` | `StartOutboxRelay` and `StartScheduler` background loops. |
| `internal/tracing` | Optional OTLP HTTP exporter setup for API, worker, and scheduler. |
| `internal/metrics` | Prometheus metric definitions (counters and histograms). |

The **Dockerfile** builds one image with `/app/api`, `/app/worker`, and `/app/scheduler`; Compose selects the command per service.

---

## Data model

Schema is defined under `migrations/` (see [Database migrations](#database-migrations)). Notable entities:

- **`notifications`**: Core row with `recipient`, `channel` (`sms` \| `email` \| `push`), `priority` (`high` \| `normal` \| `low`), `status`, optional `batch_id`, optional `idempotency_key` (partial unique index), optional `scheduled_at`, `correlation_id`, `provider_message_id`, either inline `content` or `template_id` + `payload` (JSONB).
- **`notification_batches`**: Optional header for batch creates (metadata JSON + count).
- **`templates`**: Named template per channel; body supports `{{key}}` placeholders.
- **`outbox`**: Stores failed AMQP publish attempts for asynchronous retry (`published_at` NULL until relay succeeds).

---

## Notification lifecycle

| Status | Meaning |
| ------ | ------- |
| `pending` | Persisted; not yet successfully published to RabbitMQ, or waiting for `scheduled_at`. |
| `queued` | Message published to RabbitMQ; awaiting worker. |
| `sending` | Worker claimed the message and passed pre-send validation; delivery in progress / retrying. |
| `delivered` | Webhook returned success (2xx); optional `messageId` stored. |
| `failed` | Permanent validation/delivery failure or retries exhausted. |
| `cancelled` | Terminal; only from `pending` or `queued` via cancel API. |

Workers **skip** processing when the row is already `cancelled` or `delivered`.

---

## RabbitMQ topology

| Name | Type | Purpose |
| ---- | ---- | ------- |
| `notifications.topic` | topic | Publishes work with routing key `notify.{channel}.{priority}`. |
| `q.notify.sms`, `q.notify.email`, `q.notify.push` | classic queue | One queue per channel; `x-max-priority=10` maps API priority to RabbitMQ message priority (high=10, normal=5, low=1). |
| `notifications.dlx` | direct | Dead-letter exchange. |
| `q.notify.dlq` | queue | Binds to `notify.dlq`; receives messages nacked without requeue from work queues. |
| `notifications.status` | fanout | Workers publish JSON status events; API binds an **exclusive, auto-delete** queue per connection for WebSocket fan-out. |

**Prefetch**: Consumers use QoS prefetch (10) so multiple deliveries can be in flight while the per-channel rate limiter shapes actual webhook throughput.

---

## Message envelope and tracing

Published work messages are JSON envelopes (`internal/amqp.Envelope`) including `id`, `recipient`, `channel`, optional `content`, optional `template_id` / `payload`, `priority`, and optional `correlation_id`.

**OpenTelemetry**: When a tracer is configured, the API **injects** W3C trace context into AMQP headers on publish. Workers **extract** context before handling a delivery and attach an instrumented HTTP transport to the webhook client, so traces can span API → broker → worker → downstream HTTP when an OTLP endpoint is available.

---

## API

Base path: **`/v1`**. Correlation: prefer header **`X-Request-ID`**; Chi `RequestID` middleware also assigns one and echoes it on the response.

| Method | Path | Description |
| ------ | ---- | ----------- |
| `POST` | `/v1/notifications` | Create one or more notifications (max **1000** per request). Supports `batch_metadata` when more than one item (creates a batch). |
| `GET` | `/v1/notifications` | List with filters: `status`, `channel`, `from`, `to` (RFC3339), `limit` (1–100), cursor `next_cursor` / `cursor` pagination. |
| `GET` | `/v1/notifications/{id}` | Get single notification by UUID. |
| `POST` | `/v1/notifications/{id}/cancel` | Cancel if `pending` or `queued`. |
| `GET` | `/v1/batches/{batchId}/notifications` | List all notifications in a batch. |
| `POST` | `/v1/templates` | Create a named template (409 on duplicate `name`). |
| `GET` | `/v1/ws` | WebSocket upgrade; query `notification_id` and/or `batch_id`. |

**Health and metrics (unversioned)**

| Method | Path | Description |
| ------ | ---- | ----------- |
| `GET` | `/healthz` | Liveness. |
| `GET` | `/readyz` | Readiness: PostgreSQL ping + AMQP connectivity check. |
| `GET` | `/metrics` | Prometheus exposition (API process). |
| `GET` | `/swagger/*` | Swagger UI (generated spec). |

### Content validation (summary)

- **SMS**: single body string; max length enforced (see `internal/domain/notification.go`).
- **Email**: first line of `content` = subject, remainder = body; subject and body size limits enforced.
- **Push**: first line = title, optional second segment = body; length limits enforced.
- Either **`content`** or **`template_id` + `payload`** is required per item.

### Idempotency

If `idempotency_key` is provided and duplicates an existing row, the API returns the stored notification with `inserted: false` in the response shape and does **not** enqueue a duplicate message.

### Example: create immediate SMS

The first example is minimal. The second repeats the same payload with an optional **`idempotency_key`**: if the client retries after a timeout or network error, a duplicate key returns the existing row (`inserted: false`) and does **not** publish another broker message.

**Minimal**

```bash
curl -sS -X POST "http://localhost:8080/v1/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "notifications": [{
      "recipient": "+905551234567",
      "channel": "sms",
      "content": "Your message",
      "priority": "normal"
    }]
  }'
```

**With idempotency (recommended for production sends)**

```bash
curl -sS -X POST "http://localhost:8080/v1/notifications" \
  -H 'Content-Type: application/json' \
  -H 'X-Request-ID: req-order-991' \
  -d '{
    "notifications": [{
      "recipient": "+905551234567",
      "channel": "sms",
      "content": "Your message",
      "priority": "normal",
      "idempotency_key": "order-991-sms-receipt"
    }]
  }'
```

### Example: webhook.provider contract

The reference integration expects:

`POST {WEBHOOK_URL}`

```json
{ "to": "+905551234567", "channel": "sms", "content": "Your message", "priority": "normal" }
```

A typical simulated success response is **HTTP 202** with:

```json
{ "messageId": "uuid-here", "status": "accepted", "timestamp": "ISO8601" }
```

The client accepts any **2xx** and records `messageId` when present.

---

## Templates

1. `POST /v1/templates` with `name`, `body`, `channel`, optional `variables_schema` (JSON).
2. Create notifications with `template_id` and `payload` as a JSON object whose keys match `{{key}}` placeholders in the template body.
3. Workers load the template from PostgreSQL and render text **before** calling the webhook, so the provider receives a plain `content` string.

---

## Scheduled notifications

Set `scheduled_at` (RFC3339) on create. The API **does not** publish to RabbitMQ until the timestamp is due. The **`scheduler` service** runs **`StartScheduler`** (see `internal/runner/background.go`): every 2s it selects `pending` rows with `scheduled_at <= now()`, publishes them, and transitions them to `queued`. This avoids a RabbitMQ delayed-message plugin and keeps scheduling authoritative in PostgreSQL.

---

## Outbox pattern

If `PublishNotification` fails after the HTTP transaction has committed, the API inserts into **`outbox`**. The **`scheduler` service** runs **`StartOutboxRelay`**: it polls unpublished rows, retries publish, then marks the notification `queued` and sets `outbox.published_at`. If the notification is no longer `pending`, the relay marks the outbox row published to drain orphans.

This gives **at-least-once** publication to the broker in the presence of transient broker outages (duplicate deliveries are still possible; workers rely on idempotent provider behavior or business-level idempotency keys for strict exactly-once semantics at the provider).

---

## Worker delivery and retries

1. **Eligibility**: Consumers process rows in **`pending`**, **`queued`**, or **`sending`**. Allowing **`pending`** covers a rare race where AMQP publish succeeded but the API or relay could not commit `pending`→`queued` in time; without it, the worker would acknowledge the message while skipping work and the notification could stay stuck. **`MarkQueuedWithRetry`** (API and scheduler) and a transactional retry in the outbox relay reduce how often this happens.
2. **Validation**: Template/channel mismatch or bad substitution → **permanent** → row `failed`, message **acked**, status event published.
3. **Rate limit**: Per-channel **100 req/s** — cluster-wide when **`REDIS_URL`** is set, otherwise per worker process (in-memory).
4. **Webhook**:
   - **2xx**: `delivered`, store `provider_message_id` if returned.
   - **429**, **5xx**, network errors: **transient** → exponential backoff (base 250ms, cap 30s, small jitter), **republish** same body with incremented `x-retry-count` header, **ack** original message (avoids unbounded local redelivery loops). After **5** retries, mark `failed`.
   - Other **4xx**: **permanent** → mark `failed`, **ack**.

Malformed AMQP payloads or poison messages may still be **nacked** without requeue and land in **`q.notify.dlq`** depending on consumer behavior. If `mark queued after publish` errors appear in API logs, inspect the row and queue; the worker path above is the safety net.

---

## Real-time status (WebSocket)

Workers publish JSON to **`notifications.status`** after terminal or relevant transitions, for example:

```json
{
  "notification_id": "uuid",
  "status": "delivered",
  "batch_id": "optional-uuid",
  "correlation_id": "optional"
}
```

The API process consumes these events on a dedicated queue and pushes them to WebSocket clients subscribed via:

`GET /v1/ws?notification_id=<uuid>&batch_id=<uuid>` (at least one query parameter required).

---

## Observability

### Metrics (Prometheus)

Defined in `internal/metrics/metrics.go` (API registers the default registry; worker may expose its own registry on `METRICS_ADDR`):

| Metric | Type | Labels | Description |
| ------ | ---- | ------ | ----------- |
| `notifystream_notifications_sent_total` | counter | `channel` | Successful webhook responses (2xx). |
| `notifystream_notifications_failed_total` | counter | `channel`, `outcome` | Failures (`permanent`, `retries_exhausted`, `validation`, `unknown`, …). |
| `notifystream_delivery_latency_seconds` | histogram | `channel` | Worker time from pickup through webhook response. |
| `notifystream_rate_limit_wait_seconds` | histogram | — | Time blocked on rate limiter. |

**Note:** A **queue depth** gauge backed by the RabbitMQ management HTTP API is listed in the original product brief; the current codebase focuses on the metrics above. You can extend the API or a sidecar to scrape `http://rabbitmq:15672/api/queues` if required.

### Logging

Both binaries use **`log/slog`** with JSON output. HTTP access logs include **`correlation_id`** (from `X-Request-ID` / Chi request ID). Workers attach `correlation_id` and `notification_id` when present on the AMQP envelope or headers.

### Tracing

If `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set, the API wraps the router with `otelhttp` and the worker uses `otelhttp.NewTransport` for outbound calls, with trace propagation through AMQP headers as described above.

Exporter implementation: **`otlptracehttp`** (`internal/tracing/tracing.go`) — use an **HTTP** OTLP endpoint (Jaeger’s OTLP HTTP listener is **4318**). **Do not open `http://localhost:4318/` in a browser** — that port is for the OTLP protocol only (you will often see `404 page not found`). The **Jaeger UI** is **[http://localhost:16686](http://localhost:16686)**.

#### Local Jaeger (Docker Compose)

1. **Docker Compose** injects `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318` for `api`, `worker`, and `scheduler` when the variable is unset, so **`notifystream-api` appears in Jaeger after traffic** without editing `.env`. To run **without** tracing, put `OTEL_EXPORTER_OTLP_ENDPOINT=` (empty) in `.env`, or remove the `OTEL_EXPORTER_OTLP_ENDPOINT` lines under `environment` in [`docker-compose.yml`](docker-compose.yml). For **host-run** binaries (`go run`), set the same variable in [`.env`](.env.example) (e.g. `http://127.0.0.1:4318` if Jaeger listens on the host).

2. Start the stack (Jaeger is included by default):

   ```bash
   docker compose up --build
   ```

3. Open **Jaeger UI**: [http://localhost:16686](http://localhost:16686) → **Search** (path **`/search`**). **`/trace` alone is not the search page** — it is used with a trace ID (`/trace/<id>`) after you open a trace from search results.

4. Generate traffic (e.g. `POST /v1/notifications`) and refresh the UI; you should see spans linked across API → (optional publish span context) → worker → webhook HTTP.

5. To see the HTTP path the API handled, open a span in the trace timeline and check **Tags** for attributes such as **`http.target`**, **`http.route`** (when available), or **`url.full`**.

**Reading the Search results:** Traces whose service is **`jaeger-all-in-one`** with **`http.target=/api/traces`** come from the **Jaeger UI** calling its own query API on port **16686** — not from NotifyStream. For Swagger or `curl` against **`localhost:8080`**, set **Service** to **`notifystream-api`**, then **Find Traces** (you can narrow by operation or tags if the UI offers it).

**If the UI does not load:** Jaeger only runs when its container is up. Run `docker compose up -d` (or recreate after pulling the image), then `docker ps` and confirm a container is listening on **16686**. Another process using port 16686 will prevent the UI from starting.

#### Jaeger without Compose (one container)

```bash
docker run -d --name jaeger \
  -p 16686:16686 -p 4318:4318 \
  -e COLLECTOR_OTLP_ENABLED=true \
  jaegertracing/all-in-one:1.57
```

Then point processes on the host at `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`. If the API/worker run **inside** Compose but Jaeger runs on the host, use `http://host.docker.internal:4318` (Docker Desktop) instead of `127.0.0.1`.

---

## Configuration

| Variable | Required | Default | Description |
| -------- | -------- | ------- | ----------- |
| `DATABASE_URL` | yes | — | PostgreSQL DSN (e.g. `postgres://user:pass@host:5432/db?sslmode=disable`). |
| `AMQP_URL` | yes | — | RabbitMQ AMQP URI (e.g. `amqp://guest:guest@rabbitmq:5672/`). |
| `WEBHOOK_URL` | worker: yes | — | Full URL of the external notification provider. |
| `HTTP_ADDR` | no | `:8080` | API listen address. |
| `METRICS_ADDR` | no | empty | If set (e.g. `:9091`), worker serves `GET /metrics`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | — | OTLP endpoint for traces (HTTP), e.g. `http://jaeger:4318` when using Docker Compose (Jaeger service). |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | no | — | Alternative to the above; full traces URL if your collector requires it. |
| `OTEL_SERVICE_NAME` | no | — | Standard OTel resource attribute (also influenced by `resource.WithFromEnv()`). |
| `REDIS_URL` | no | — | If set (e.g. `redis://redis:6379`), workers share a **cluster-wide** per-channel delivery rate limit (100/s) via Redis; otherwise each worker process uses an in-memory limiter. |
| `WEBSOCKET_ALLOWED_ORIGINS` | no | — | Comma-separated **exact** `Origin` values allowed for `/v1/ws`. Empty means allow all (development). |
| `STUCK_SENDING_RECOVERY_SECONDS` | no | `120` | Worker: if a row stays **`sending`** longer than this (seconds), the next delivery attempt resets it to **`queued`** before processing. Set `0` to disable. |

Copy `.env.example` to `.env` for Docker Compose.

---

## Running with Docker Compose

```bash
cp .env.example .env
# Set WEBHOOK_URL to your https://webhook.site/<uuid>
docker compose up --build
```

- **API**: `http://localhost:8080`
- **Swagger UI**: `http://localhost:8080/swagger/index.html`
- **Redis**: `localhost:6379` (Compose default; matches `REDIS_URL=redis://redis:6379` in `.env.example`)
- **RabbitMQ management**: `http://localhost:15672` (`guest` / `guest`)
- **Worker metrics** (if `METRICS_ADDR=:9091` in `.env`): `http://localhost:9091/metrics`
- **Jaeger**: UI [http://localhost:16686](http://localhost:16686), OTLP HTTP port **4318** (set `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318` in `.env` to export traces)

The **scheduler** and **worker** services wait for the API container to **start** so migrations can run first. For replica counts, rate limits across workers, WebSocket scaling, and secrets, see **[OPERATIONS.md](OPERATIONS.md)**.

---

## Local development

Requirements: Go 1.23+, PostgreSQL, RabbitMQ (or use Compose for dependencies only).

```bash
make test        # go test ./...
make lint        # requires golangci-lint on PATH
make swagger     # regenerate docs after changing // @ annotations
```

Run **`go run ./cmd/api`**, **`go run ./cmd/scheduler`**, and **`go run ./cmd/worker`** (scheduler and worker need `DATABASE_URL` and `AMQP_URL`; worker also needs `WEBHOOK_URL`). Same variables as `.env.example`.

---

## OpenAPI / Swagger

The API contract is generated with **[swaggo/swag](https://github.com/swaggo/swag)** from comments on handlers in `internal/api` and `cmd/api`. Generated artifacts live under **`docs/`** (`docs.go`, `swagger.json`, `swagger.yaml`).

```bash
make swagger
```

Do not hand-edit `swagger.json` as the source of truth; regenerate after annotation changes.

---

## Database migrations

SQL files live in `migrations/` following golang-migrate naming (`000001_initial.up.sql` / `.down.sql`). The API invokes **`migrate.Up`** on startup (`internal/migrate`). For manual runs, use the `migrate` CLI with the same DSN if preferred.

---

## Testing and CI

- **Unit tests**: `make test` (or `go test ./...`), including domain validation and template rendering tests.
- **CI**: `.github/workflows/ci.yml` runs tests and `golangci-lint` on pushes and pull requests to `main` / `master`.

Integration tests with Testcontainers are not included in the default suite; the webhook client is intended to be tested against `httptest.Server` in CI-friendly tests if you extend coverage.

---

## License

Specify your license in a `LICENSE` file at the repository root if you distribute this project.
