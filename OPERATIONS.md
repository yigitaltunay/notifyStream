# Scaling and operational notes

- **Horizontal workers:** Each worker still targets **100 msg/s per channel**. Set **`REDIS_URL`** (e.g. `redis://redis:6379` in Compose) so all workers share one Redis-backed limiter (`internal/ratelimit` + `github.com/go-redis/redis_rate`); if `REDIS_URL` is unset, the limiter stays **in-memory per process**. `docker-compose.yml` starts **redis** by default so that URL resolves; clear `REDIS_URL` to use **in-memory** limiting per worker (Compose still runs the Redis container unless you remove or override that service).

- **Scheduler / outbox:** Outbox and due-scheduled work is claimed with **`SELECT … FOR UPDATE SKIP LOCKED`** (`internal/db/outbox_scheduler.go`, `internal/runner/background.go`), so **multiple `cmd/scheduler` replicas** can run without double-claiming the same rows. Compose still defaults to one `scheduler` service for simplicity.

- **PostgreSQL:** Listing uses cursor pagination on `(created_at DESC, id DESC)`. Migration **`000002_list_index`** adds `idx_notifications_list_filter` on `(status, channel, created_at DESC, id DESC)` for filtered lists as volume grows.

- **Secrets:** Do not commit `.env`; use your orchestrator’s secret management in production.

- **WebSocket:** The hub is in-process, but **status events** are published to a **RabbitMQ fanout** exchange and each API instance consumes on its **own ephemeral queue**, so every replica receives all status messages—no extra Redis layer required for fanout. Set **`WEBSOCKET_ALLOWED_ORIGINS`** (comma-separated exact `Origin` values) to restrict browser origins in production. Clients still need a TCP connection to the instance that accepted the upgrade (use sticky sessions at the load balancer if you care which node holds a given socket).

- **Stuck `sending`:** The worker sets `sending` before the webhook. Abort paths (rate limit, republish failure, shutdown during backoff) **revert to `queued`** and **Nack with requeue**. **`STUCK_SENDING_RECOVERY_SECONDS`** (default 120, `0` = off) resets rows that stayed `sending` longer than that when a new delivery runs. **`MarkDelivered`** after a 2xx webhook retries for up to ~45s with backoff (parent shutdown does not cancel the DB writes). If it still fails, the AMQP message is discarded without requeue to avoid duplicate SMS—repair the row manually. Legacy fix: `UPDATE notifications SET status = 'queued', updated_at = now() WHERE id = '<uuid>' AND status = 'sending';` if the queue message is gone.

---

**Code hints:** Delivery + recovery: `cmd/worker`, `internal/ratelimit`, `internal/amqp` (`ErrRequeueDelivery`). WebSocket: `internal/api/ws.go`.

See also [README.md](README.md) for architecture and [EXAMPLES.md](EXAMPLES.md) for API usage.
