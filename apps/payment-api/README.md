# payment-api

The demo workload for the agentic SRE PoC: a small payments API in Go, backed
by Postgres. Served to the SRE agent as `platform://services/payment-api/readme`.

Public URL: `https://payments.calebache.com` (through the `public` Gateway).
Manifests: [`gitops/workloads/dev/payment-api/`](../../gitops/workloads/dev/payment-api/).
Runbook: [`docs/runbooks/payment-api.md`](../../docs/runbooks/payment-api.md).

## API

| Method and path | Request | Responses |
|---|---|---|
| `POST /v1/payments` | `{"amount_cents": 1999, "currency": "CAD"}` | `201` payment · `400` invalid input · `500` database failure |
| `GET /v1/payments/{id}` | — | `200` payment · `404` unknown or malformed id · `500` database failure |
| `GET /healthz` | — | `200` while the process runs (liveness) |
| `GET /readyz` | — | `200` while the process runs (readiness) |
| `GET /metrics` (port 9090) | — | Prometheus metrics; not publicly routed |

Validation: `amount_cents` 1–10,000,000; `currency` is three upper-case
letters; unknown JSON fields are rejected.

A payment:

```json
{"id":"432409b2-6303-4373-a562-958fe89db094","amount_cents":1999,"currency":"CAD","status":"captured","created_at":"2026-10-17T13:55:33Z"}
```

Schema: [`schema.sql`](schema.sql) (embedded and applied idempotently at
startup, also served as `platform://services/payment-api/schema`).

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_HOST` | — (required) | `postgres.payment-api.svc.cluster.local` in dev |
| `DATABASE_PORT` | `5432` | |
| `DATABASE_NAME` | `payments` | |
| `DATABASE_USER`, `DATABASE_PASSWORD` | — (required) | From Secret `payment-api-db` (External Secrets ← Secrets Manager `agentic-sre-dev/payment-api/db`) |
| `LISTEN_ADDR` / `METRICS_ADDR` | `:8080` / `:9090` | |

## Behaviour that matters for incidents

- **Health checks never touch the database.** A broken database connection
  does not make pods unready; the rollout completes and requests fail with
  HTTP 500. This is deliberate, so the failure is visible to monitoring.
- The database connection is lazy. A wrong `DATABASE_HOST` does not crash the
  process; it logs a warning at startup and every database request fails.
- **Malformed IDs are 404s**, never 5xx, so junk traffic cannot trip the
  error-rate alert.
- Clients only ever see `{"error":"internal error"}`; the real cause is in the
  logs.

## Telemetry

Metrics (port 9090):

| Metric | Labels | Meaning |
|---|---|---|
| `http_requests_total` | `route`, `method`, `code` | Requests by route pattern (not raw path) |
| `http_request_duration_seconds` | `route`, `method` | Latency histogram |
| `payment_database_errors_total` | — | Database operations that failed |

Logs are JSON on stdout. Each request logs `request_id`, `route`, `status`
and `duration_ms`. A database failure logs:

```json
{"level":"ERROR","msg":"database operation failed","op":"create payment","request_id":"e6a4346433a19c18","error":"failed to connect to `user=payments database=payments`: ..."}
```

Alerts (`gitops/workloads/dev/payment-api/monitoring.yaml`):

- `PaymentApiHighErrorRate` — 3 or more non-probe requests returned 5xx in the last minute (evaluated every 15 s, fires immediately).
- `PaymentApiDown` — no instance scraped successfully for 2 minutes.

## Load generator

The same image runs in `loadgen` mode as the `payment-loadgen` Deployment,
sending a create-then-read pair every 500 ms through the public URL, so the
alert always has traffic to measure. Its logs summarise status counts every
30 seconds.

## Develop

```bash
go test -race ./...                # unit tests
scripts/build-push.sh payment-api  # from the repo root; tag = commit SHA
```

Then set the new tag in `deployment.yaml` and `loadgen.yaml` and commit.
