# RavenHub

## Language / Idioma

**English** (default) · [Español](README.es.md)

A centralized Notification Hub: a single service that your agents and
workflows (ZCode, OpenCode, n8n, LangGraph, CrewAI, scripts, cron…) call with
**one standardized HTTP request** to send notifications. RavenHub
authenticates, routes, renders, delivers, retries, rate-limits and audits.

- **Multichannel**: Telegram, Email, Webhook, Discord, Slack and `log` (dry-run).
- **Multi-account**: several Telegram bots, several SMTP accounts… managed in one place.
- **Multi-tenant**: each project has its own token, accounts and destinations, isolated by design. Channel secrets live **only in the Hub**.

```
[Agent / Workflow] ──►  POST /v1/notify  ──►  RavenHub ──► Telegram (account 1, account 2…)
                                       (queue + retries + limits)  ──► Email / Webhook / …
```

## ⚠️ AI Disclosure

This project was developed with assistance from artificial intelligence tools. Given the automated nature of some components, users are advised to review and test the code independently before integrating it into their own systems.

## Requirements

- **Go 1.26.10** (`go.mod` pins the toolchain).
- **Docker + Docker Compose** for Postgres, the containerized Hub and the integration suite.
- **Python 3.10+** only if you use the SDK (`sdk/`).
- **age** for encrypted backups (`make backup`); without a key, backups fail by default.
- For real channels: a Telegram bot and/or an SMTP account with an app password.

## Quickstart (local, 3 commands)

```bash
make dev          # Postgres in Docker + .env with generated master key and admin token
make seed         # if dev/seed.yaml is missing, copies the log-only DEMO (project demo, token demo, log:dev)
make run          # RavenHub at http://localhost:8085 (make dev generates master key/admin token if missing)
```

`make seed` prints the token **once**. Use it with:

```bash
curl -s localhost:8085/v1/notify -H "Authorization: Bearer rvn_demo_..." \
  -H 'Content-Type: application/json' \
  -d '{"to":["log:dev"],"title":"Hello","message":"first notification"}'
```

For real channels (Telegram/SMTP), copy `seed.example.yaml` to
`dev/seed.yaml`, fill the `${VARS}` in `.env` or in `.env.seed` (recommended:
600, ignored) and run `make seed` again. The DEMO seed needs no external
credentials.

Full smoke test (14 end-to-end checks, with cleanup):

```bash
make smoke        # requires the hub running and RAVENHUB_ADMIN_TOKEN in .env
make status       # diagnostics: hub/DB/config/backup/logs (exit ≠ 0 if the hub is down)
```

## The model: channel → account → destination (named)

- **Channel**: `telegram`, `email`, `log`, `webhook`, `discord`, `slack`.
- **Account**: a concrete credential (a bot, an SMTP account), encrypted at rest (AES-256-GCM).
- **Destination**: account + recipient with a **unique name per project** (`telegram:alerts`, `email:ops`).
- **Group**: an alias that delivers to **all** its destinations — fan-out (redundancy by duplication, not conditional failover).

The endpoint receives **destination names, never raw chat_ids** — an agent
cannot send to an arbitrary chat, only to what is preconfigured in the Hub.

**The project is the trust boundary:** a token without `scopes.destinations`
can read all messages in its project. If you need to isolate agents from each
other, use one project per consumer (or destination scopes).

## API

### Send

```http
POST /v1/notify
Authorization: Bearer rvn_<project>_<...>
Idempotency-Key: <optional, recommended>

{
  "to": ["oncall"],                  // destinations or groups in the project
  "priority": "high",                // low | normal | high | urgent
  "title": "Pipeline error",
  "message": "Agent X failed...",
  "template": "error_pipeline",      // optional: server-side template
  "data": {"service": "scraper"},
  "tags": ["error", "prod"]
}
```

→ `202 {"id": "<uuid>"}` (delivery is asynchronous) · `?sync=true` waits up to `SyncWait` (8s by default): returns `200` if the message reaches a terminal state; otherwise `202`.
Errors: `400` validation · `401` token · `403` scopes (destinations/priority) · `429` rate limit or shedding with `Retry-After` · `503` queue saturated.
The `202` does not mean "delivered": the real result is in `GET /v1/messages/{id}` (status + `deliveries` per destination).

### Query / operate

```
GET  /v1/messages/{id}                      status + deliveries per destination
GET  /v1/messages?status=dead               history (scoped to the project)
GET  /healthz /readyz /metrics              operations (Prometheus)
POST /admin/*                               projects, tokens, accounts, destinations,
                                            groups, templates, DLQ replay, destination test
```

## Lifecycle and guarantees

- **Retries** with exponential backoff + per-priority jitter (urgent is more aggressive): 4 attempts (low), 5 (normal), 6 (high/urgent) — and the full budget **fits inside the priority TTL** (invariant covered by tests).
- **TTL**: a "pipeline down" notification is useless 3h later — urgent 15m, high/normal 1h, low 24h (configurable per project with `options.ttl`). A channel down longer than the TTL leaves the loss **visible** (`expired` status, `ravenhub_expired_total`, `message.expired` audit entry, counter in `/readyz`), never silent. If a retry does not fit before `expires_at`, the message goes to the **DLQ** with an explicit reason instead of expiring.
- **Idempotency**: `(project, Idempotency-Key)` is unique **while the message row is alive** — terminal messages are kept 30 days by default (`RAVENHUB_RETENTION_MESSAGES`), non-terminal ones are not deleted. Resending with the same key does not duplicate; for "one per period" use deterministic keys (SDK: `idempotency_key_for(period="day")`).
- **Retention**: terminal messages 30 days, audit 90 (configurable with `RAVENHUB_RETENTION_*`; the audit log is append-only except for that sweep).
- **Queue on PostgreSQL** (`FOR UPDATE SKIP LOCKED`, batch claims): no Redis/RabbitMQ to operate. A process crash does not lose messages (lease reaper); delivery is **at-least-once** (a crash may duplicate). The only possible loss is due to an expired TTL and is visible as `expired`.
- **Two-layer rate limiting**: per-project ingress (token bucket) + per-destination egress based on real provider limits. Pacing **does not burn retries**.
- **Backpressure**: full queue → `429` for low/normal (shedding) and `503` at the hard limit; a hard per-project quota (`RAVENHUB_MAX_QUEUE_PROJECT`) prevents one tenant from blocking the others. Growth degrades, never crashes. Every rejection increments `ravenhub_shed_total{scope,code}` (project/global/intake).
- **Quiet hours** per destination (with timezone): `normal`/`low` **and `high`** are deferred to business hours; only `urgent` passes through. Deferred messages are delivered later (the TTL shifts with the deferral).
- **Visible truncation**: if content is cut by the channel limit (Telegram 4096, Discord 2000, Slack 4000, log 8000), the delivery is marked (`deliveries[].truncated` + `ravenhub_truncated_total`).
- **Server-side templates** with plain-text fallback (better to notify ugly than not to notify).
- **Append-only audit log with two controlled exceptions** (retention sweep via DELETE and project anonymization via UPDATE with `privacy_purge`): `notify`, `notify.aggregated` (with `count`), `message.expired`, `message.destination_missing`, `message.replayed`, `token.scopes_updated`, `project.options_updated`…

### What each state means

| State | Meaning |
|---|---|
| `queued` / `delivering` | Queued / in progress. |
| `sent` | **All** per-destination deliveries succeeded. |
| `failed` | At least one destination failed permanently (the others may have delivered: details are in `deliveries`). |
| `dead` | Retries exhausted, or the next retry did not fit in the TTL → DLQ (replayable with `POST /admin/messages/{id}/replay`). |
| `expired` | TTL expired before delivery (replayable like the DLQ). |

## Security and privacy

- Prefixed tokens (`rvn_project_...`), stored **only as SHA-256**, with scopes (allowed destinations + maximum priority); instant revocation.
- Channel secrets encrypted at rest with AES-256-GCM; master key outside the DB (`RAVENHUB_MASTER_KEY`).
- TLS terminates at Caddy in production; `/metrics` and `/readyz` stay on the Docker network (the public vhost returns 404). Without `RAVENHUB_ADMIN_TOKEN` the Hub refuses to start unless `RAVENHUB_DEV=1` (strict boolean: `0`/`false` do not enable dev).
- Backups **encrypted with age, mandatory**: without `AGE_PUBLIC_KEY`, `make backup` fails by default. Docker logs rotate (10 MB × 3); the `log` channel is dry-run and by default only records `body_len` (full body requires `RAVENHUB_LOG_CHANNEL_BODY=1`).
- Retention: terminal messages 30 days, audit 90; deleting a project anonymizes its audit rows (no project/destination/prefix/detail) and leaves a global `project.deleted` tombstone (documented decision).
- Vulnerability reporting: [SECURITY.md](SECURITY.md).

## Consuming from agents

- **curl / n8n**: one HTTP call (see above).
- **Python SDK** (`sdk/`, installable with `pip install ./sdk`; **no mandatory dependencies**: uses `requests` if present, otherwise `urllib`):

```python
from ravenhub import notify, notify_safe, idempotency_key_for  # export NOTIFY_TOKEN=rvn_...

notify("Deploy OK", "prod updated", to=["oncall"], priority="high")

# Agent that must not break (cron, autonomous loop): best-effort.
# Never raises; without config it is a no-op with a warning; leaves a trace in the log.
notify_safe("Degraded reflection", "...", idempotency_key=idempotency_key_for("reflect", period="day"))
```

Error contract: `notify()`/`status()` raise `NotifyError` on any failure (HTTP, network, timeout, redirect — redirects are **not** followed: the token does not jump hosts). `configured()` is the explicit gate.

- **MCP** (ZCode, OpenCode, Claude, CrewAI…): `ravenhub mcp` exposes the `notify` tool over stdio:

```json
{"mcpServers": {"ravenhub": {"command": "ravenhub", "args": ["mcp"],
  "env": {"RAVENHUB_URL": "http://localhost:8085", "NOTIFY_TOKEN": "rvn_project1_..."}}}}
```

- **Admin CLI**: `ravenctl dev/seed.yaml` applies the declarative seed;
  `ravenhub serve|migrate|import|mcp|re-encrypt|version` are subcommands of the same codebase.

## Development

```bash
make help              # all targets
make test              # Go unit tests
make test-race         # unit tests with the race detector
make test-sdk          # Python SDK unit tests (no network)
make test-integration  # 45 e2e tests against an ephemeral Postgres (includes a 1,000-message burst)
make status            # the "doctor": hub, DB, config, backup and logs
make watchdog          # local hub watchdog (log-only alerts without a bot)
make lint && make build
```

CI: [`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs `go vet`, unit+race, SDK tests and `docker compose config`. The ephemeral-Postgres integration suite remains local (`make test-integration`).

Stack: **Go** (same codebase; `ravenhub` service binary + `ravenctl` CLI) + **PostgreSQL** + Docker Compose (`compose.dev.yaml` local-first, `compose.prod.yaml` with Caddy+watchdog for the VPS).

License: [MIT](LICENSE).
