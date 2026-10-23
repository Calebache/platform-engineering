# Runbook: payment-api

Served to the SRE agent as `platform://services/payment-api/runbook`.
Service reference: [`apps/payment-api/README.md`](../../apps/payment-api/README.md).

## Alerts

| Alert | Fires when |
|---|---|
| `PaymentApiHighErrorRate` | 3 or more API requests (probes excluded) returned 5xx in the last minute; fires immediately, no `for` delay |
| `PaymentApiDown` | No payment-api instance has been scraped successfully for 2 minutes |

## Triage

1. `get_incident_context(service="payment-api")` — health, error ratio, top
   error patterns, recent deployments, recent commits, in one call.
2. If the error ratio is high, check `get_error_patterns`. Nearly every
   payment-api 5xx is a database failure; the log message is
   `database operation failed` and the `error` field names the cause.
3. Correlate the time the errors started with `get_recent_deployments`
   (which revision, which env vars changed) and `get_git_changes`.
4. Pull raw lines with `get_recent_logs(request_id=...)` only to confirm.

## Known causes

Match the `error` field of `database operation failed` log lines:

| Error contains | Cause | Remediation |
|---|---|---|
| `lookup <host>` / `no such host` / `hostname resolving error` | `DATABASE_HOST` points at a host that doesn't exist, usually from a recent commit to `gitops/workloads/dev/payment-api/deployment.yaml` | PR restoring `DATABASE_HOST` to `postgres.payment-api.svc.cluster.local` (revert the offending change). Agent may fix. |
| `connection refused` / `dial tcp ...:5432` timeout | Postgres is down or restarting | Check `postgres-0` in `get_service_health`. If it's recovering, wait; data is ephemeral in dev. If it's not starting, escalate. |
| `password authentication failed` / `failed SASL auth` | The app's password (Secrets Manager → External Secret) doesn't match the one inside Postgres, usually after a rotation | **Escalate to a human.** Secrets are rotated in Secrets Manager and inside Postgres, not through Git. The agent must not attempt this. |
| `relation "payments" does not exist` | Schema not applied | Restart payment-api (applies `schema.sql`); escalate if it persists. |
| Anything else | Unknown | Escalate with the evidence gathered. |

Pods staying Ready while requests fail is expected: health checks
deliberately ignore the database.

## Remediation rules

- Never change the cluster directly. Fixes are pull requests against
  `gitops/workloads/dev/payment-api/` on an `agent/*` branch via
  `create_pull_request`.
- Make the minimal change: revert the offending value, nothing else.
- The PR body must state the hypothesis, the evidence (error pattern, counts,
  the deployment and commit it started with), and how recovery will be
  verified.
- Never put credentials in a PR. Secret problems are escalated.

## Verify recovery

After Argo CD deploys the fix (`get_service_health` shows the new revision
synced and pods Ready):

- error ratio from `get_metrics` back under 1% for at least 5 minutes;
- no new `database operation failed` patterns;
- `PaymentApiHighErrorRate` resolved.

Only then is the incident resolved.
