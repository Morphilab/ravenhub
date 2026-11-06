# RavenHub

## Idioma / Language

[English](README.md) (default) · **Español**

Notification Hub centralizado: un único servicio al que todos tus agentes y
workflows (ZCode, OpenCode, n8n, LangGraph, CrewAI, scripts, cron…) envían
notificaciones con **una sola llamada HTTP estandarizada**. RavenHub
autentica, enruta, renderiza, entrega, reintenta, limita y audita.

- **Multicanal**: Telegram, Email, Webhook, Discord, Slack y `log` (dry-run).
- **Multicuenta**: varios bots de Telegram, varias cuentas SMTP… gestionadas en un solo lugar.
- **Multi-tenant**: cada proyecto tiene su token, sus cuentas y sus destinos, aislados por diseño. Los secretos de los canales viven **solo en el Hub**.

```
[Agente / Workflow] ──►  POST /v1/notify  ──►  RavenHub ──► Telegram (cuenta 1, cuenta 2…)
                                         (cola + reintentos + límites)  ──► Email / Webhook / …
```

## ⚠️ AI Disclosure / Divulgación de IA

This project was developed with assistance from artificial intelligence tools. Given the automated nature of some components, users are advised to review and test the code independently before integrating it into their own systems.

**Traducción:** Este proyecto fue desarrollado con asistencia de herramientas de inteligencia artificial. Dada la naturaleza automatizada de algunos componentes, se recomienda revisar y probar el código de forma independiente antes de integrarlo en sus propios sistemas.

## Requisitos

- **Go 1.26.10** (el `go.mod` fija el toolchain).
- **Docker + Docker Compose** para Postgres, el Hub en contenedor y la integración.
- **Python 3.10+** solo si usas el SDK (`sdk/`).
- **age** para backups cifrados (`make backup`); sin clave, el backup falla por defecto.
- Para canales reales: bot de Telegram y/o cuenta SMTP con app password.

## Inicio rápido (local, 3 comandos)

```bash
make dev          # Postgres en Docker + .env con master key y admin token generados
make seed         # si falta dev/seed.yaml copia el DEMO log-only (proyecto demo, token demo, log:dev)
make run          # RavenHub en http://localhost:8085 (make dev genera master key/admin token si faltan)
```

El `make seed` imprime el token **una sola vez**. Con él:

```bash
curl -s localhost:8085/v1/notify -H "Authorization: Bearer rvn_demo_..." \
  -H 'Content-Type: application/json' \
  -d '{"to":["log:dev"],"title":"Hola","message":"primera notificación"}'
```

Para canales reales (Telegram/SMTP), copia `seed.example.yaml` a
`dev/seed.yaml`, rellena las `${VARS}` en `.env` o en `.env.seed` (recomendado:
600, ignorado) y vuelve a ejecutar `make seed`. El seed DEMO no requiere
credenciales externas.

Smoke test completo (14 comprobaciones end-to-end, con autolimpieza):

```bash
make smoke        # requiere el hub corriendo y RAVENHUB_ADMIN_TOKEN en .env
make status       # diagnóstico: hub/DB/config/backup/logs (exit ≠ 0 si el hub no responde)
```

## El modelo: canal → cuenta → destino (nombrado)

- **Canal**: `telegram`, `email`, `log`, `webhook`, `discord`, `slack`.
- **Cuenta**: una credencial concreta (un bot, un SMTP), cifrada en reposo (AES-256-GCM).
- **Destino**: cuenta + destinatario con **nombre único por proyecto** (`telegram:alerts`, `email:ops`).
- **Grupo**: alias que entrega a **todos** sus destinos — fan-out (redundancia por duplicación, no failover condicional).

El endpoint recibe **nombres de destinos, nunca chat_ids crudos** — un agente
no puede enviar a un chat arbitrario, solo a lo preconfigurado en el Hub.

**El proyecto es la frontera de confianza:** un token sin `scopes.destinations`
puede leer todos los mensajes de su proyecto. Si necesitas aislar agentes entre
sí, usa un proyecto por consumidor (o scopes de destino).

## API

### Enviar

```http
POST /v1/notify
Authorization: Bearer rvn_<proyecto>_<...>
Idempotency-Key: <opcional, recomendado>

{
  "to": ["oncall"],                  // destinos o grupos del proyecto
  "priority": "high",                // low | normal | high | urgent
  "title": "Error en pipeline",
  "message": "El agente X falló...",
  "template": "error_pipeline",      // opcional: plantilla server-side
  "data": {"service": "scraper"},
  "tags": ["error", "prod"]
}
```

→ `202 {"id": "<uuid>"}` (el envío es asíncrono) · `?sync=true` espera hasta `SyncWait` (8s por defecto): responde `200` si el mensaje llega a estado terminal; si no, `202`.
Errores: `400` validación · `401` token · `403` scopes (destinos/prioridad) · `429` rate limit o shedding con `Retry-After` · `503` cola saturada.
El `202` no es "entregado": el resultado real está en `GET /v1/messages/{id}` (estado + `deliveries` por destino).

### Consultar / operar

```
GET  /v1/messages/{id}                      estado + entregas por destino
GET  /v1/messages?status=dead               histórico (scoped al proyecto)
GET  /healthz /readyz /metrics              operación (Prometheus)
POST /admin/*                               proyectos, tokens, cuentas, destinos,
                                            grupos, plantillas, replay de DLQ, test de destino
```

## Ciclo de vida y garantías

- **Reintentos** con backoff exponencial + jitter por prioridad (urgent más agresivo): 4 intentos (low), 5 (normal), 6 (high/urgent) — y el presupuesto completo **cabe dentro del TTL** de la prioridad (invariante verificado en tests).
- **TTL**: una notificación de "pipeline caído" no sirve 3h después — urgent 15m, high/normal 1h, low 24h (configurable por proyecto con `options.ttl`). Un canal caído más tiempo que el TTL deja la pérdida **visible** (estado `expired`, `ravenhub_expired_total`, auditoría `message.expired`, contador en `/readyz`), nunca muda. Si un reintento no cabe antes de `expires_at`, el mensaje va a **DLQ** con motivo explícito en vez de expirar.
- **Idempotencia**: `(proyecto, Idempotency-Key)` única **mientras viva la fila** del mensaje — los terminales se conservan 30 días por defecto (`RAVENHUB_RETENTION_MESSAGES`), los no-terminales no se borran. Reenviar con la misma clave no duplica; para "una por periodo" usa claves deterministas (SDK: `idempotency_key_for(period="day")`).
- **Retención**: mensajes terminales 30 días, auditoría 90 (configurable con `RAVENHUB_RETENTION_*`; la auditoría es append-only salvo ese barrido).
- **Cola sobre PostgreSQL** (`FOR UPDATE SKIP LOCKED`, claims por lotes): sin Redis/RabbitMQ que operar. Un crash del proceso no pierde mensajes (reaper de leases); la entrega es **at-least-once** (un crash puede duplicar). La única pérdida posible es por TTL vencido y queda visible como `expired`.
- **Rate limit en dos capas**: entrada por proyecto (token bucket) + salida por destino según límites reales del proveedor. El pacing **no quema reintentos**.
- **Contrapresión**: cola llena → `429` para low/normal (shedding) y `503` en el límite duro; cuota dura por proyecto (`RAVENHUB_MAX_QUEUE_PROJECT`) para que un tenant no bloquee a los demás. Crecer degrada, nunca crashea. Cada rechazo suma `ravenhub_shed_total{scope,code}` (proyecto/global/intake).
- **Quiet hours** por destino (con timezone): `normal`/`low` **y `high`** se difieren al horario hábil; solo `urgent` atraviesa. Lo diferido se entrega después (el TTL se desplaza con el aplazamiento).
- **Truncado visible**: si el contenido se corta por el límite del canal (Telegram 4096, Discord 2000, Slack 4000, log 8000), la entrega queda marcada (`deliveries[].truncated` + `ravenhub_truncated_total`).
- **Plantillas server-side** con fallback a texto plano (mejor notificar feo que no notificar).
- **Auditoría append-only con dos excepciones controladas** (barrido de retención por DELETE y anonimización de proyecto por UPDATE con `privacy_purge`): `notify`, `notify.aggregated` (con `count`), `message.expired`, `message.destination_missing`, `message.replayed`, `token.scopes_updated`, `project.options_updated`…

### Qué significa cada estado

| Estado | Significado |
|---|---|
| `queued` / `delivering` | En cola / en curso. |
| `sent` | **Todas** las entregas por destino terminaron bien. |
| `failed` | Al menos un destino falló de forma permanente (los demás pueden haber entregado: el detalle está en `deliveries`). |
| `dead` | Reintentos agotados, o el siguiente reintento no cabía en el TTL → DLQ (replayable con `POST /admin/messages/{id}/replay`). |
| `expired` | TTL vencido antes de entregar (replayable igual que la DLQ). |

## Seguridad y privacidad

- Tokens con prefijo (`rvn_proyecto_...`), almacenados **solo como SHA-256**, con scopes (destinos permitidos + prioridad máxima); revocación instantánea.
- Secretos de canales cifrados en reposo con AES-256-GCM; master key fuera de la DB (`RAVENHUB_MASTER_KEY`).
- TLS lo termina Caddy en producción; `/metrics` y `/readyz` quedan en la red de Docker (el vhost público responde 404). Sin `RAVENHUB_ADMIN_TOKEN` el Hub no arranca salvo `RAVENHUB_DEV=1` (booleano estricto: `0`/`false` no activan dev).
- Backups **cifrados con age obligatoriamente**: sin `AGE_PUBLIC_KEY`, `make backup` falla por defecto. Los logs Docker rotan (10 MB × 3); el canal `log` es dry-run y por defecto solo registra `body_len` (el cuerpo completo requiere `RAVENHUB_LOG_CHANNEL_BODY=1`).
- Retención: mensajes terminales 30 días, auditoría 90; al borrar un proyecto sus filas de auditoría se anonimizan (sin proyecto/destino/prefijo/detalle) y queda un tombstone global `project.deleted` (decisión documentada).
- Reporte de vulnerabilidades: [SECURITY.md](SECURITY.md).

## Consumo desde agentes

- **curl / n8n**: una llamada HTTP (ver arriba).
- **SDK Python** (`sdk/`, instalable con `pip install ./sdk`; **sin dependencias obligatorias**: usa `requests` si está, si no `urllib`):

```python
from ravenhub import notify, notify_safe, idempotency_key_for  # export NOTIFY_TOKEN=rvn_...

notify("Deploy OK", "prod actualizado", to=["oncall"], priority="high")

# Agente que no puede romperse (cron, ciclo autónomo): best-effort.
# Nunca lanza; sin config es no-op con warning; deja rastro en el log.
notify_safe("Reflexión degradada", "...", idempotency_key=idempotency_key_for("reflect", period="day"))
```

Contrato de errores: `notify()`/`status()` lanzan `NotifyError` ante cualquier fallo (HTTP, red, timeout, redirect — los redirects **no** se siguen: el token no salta de host). `configured()` es el gate explícito.

- **MCP** (ZCode, OpenCode, Claude, CrewAI…): `ravenhub mcp` expone la tool `notify` por stdio:

```json
{"mcpServers": {"ravenhub": {"command": "ravenhub", "args": ["mcp"],
  "env": {"RAVENHUB_URL": "http://localhost:8085", "NOTIFY_TOKEN": "rvn_proyecto1_..."}}}}
```

- **CLI admin**: `ravenctl dev/seed.yaml` aplica el seed declarativo;
  `ravenhub serve|migrate|import|mcp|re-encrypt|version` son los subcomandos de la misma base de código.

## Desarrollo

```bash
make help              # todos los targets
make test              # unitarios (Go)
make test-race         # unitarios con detector de carreras
make test-sdk          # unitarios del SDK Python (sin red)
make test-integration  # 45 e2e contra Postgres efímero (incluye ráfaga de 1.000)
make status            # el "doctor": hub, DB, config, backup y logs en orden
make watchdog          # vigilancia local del hub (alertas al log sin bot)
make lint && make build
```

CI: [`.github/workflows/ci.yml`](.github/workflows/ci.yml) ejecuta `go vet`, unit+race, tests del SDK y `docker compose config`. La integración con Postgres efímero sigue siendo local (`make test-integration`).

Stack: **Go** (misma base de código; binario de servicio `ravenhub` + CLI `ravenctl`) + **PostgreSQL** + Docker Compose (`compose.dev.yaml` local-first, `compose.prod.yaml` con Caddy+watchdog para el VPS).

Licencia: [MIT](LICENSE).
