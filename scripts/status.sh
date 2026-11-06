#!/bin/bash
# status.sh — el "doctor" del entorno dev: ¿hub, DB y config en orden?
# Solo lectura: no arranca ni modifica nada. Exit 0 = hub respondiendo.
set -uo pipefail

URL="${RAVENHUB_URL:-http://localhost:8085}"
FAIL=0
ok()   { echo "  ✓ $1"; }
bad()  { echo "  ✗ $1"; FAIL=1; }
warn() { echo "  · $1"; }

echo "· RavenHub status ($URL)"

# 1) Config local
if [ -f .env ]; then
    ok ".env presente"
    perm=$(stat -c '%a' .env 2>/dev/null || echo "?")
    if [ "$perm" = "600" ]; then ok ".env con permisos 600"; else warn ".env con permisos $perm (chmod 600 .env)"; fi
    for k in RAVENHUB_DSN RAVENHUB_MASTER_KEY RAVENHUB_ADMIN_TOKEN; do
        if grep -q "^${k}=.\+" .env; then ok "$k definido"; else warn "$k vacío en .env"; fi
    done
    if grep -q '^RAVENHUB_MASTER_KEY=dev-master-key-cambiar-en-produccion$' .env; then
        bad "RAVENHUB_MASTER_KEY es el placeholder público de example.env — rótala YA"
    fi
else
    warn ".env no existe (make up lo crea desde example.env)"
fi

# 2) Hub: liveness y readiness (readyz trae cola, DLQ y expirados)
hz=$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$URL/healthz" 2>/dev/null)
hz=${hz:-000}
if [ "$hz" = "200" ]; then
    ok "healthz 200"
else
    bad "healthz sin respuesta (código $hz) — ¿make run o make full?"
fi

rz=$(curl -s -m 3 -w $'\n%{http_code}' "$URL/readyz" 2>/dev/null)
code=$(printf '%s' "$rz" | tail -n1)
code=${code:-000}
body=$(printf '%s' "$rz" | sed '$d')
if [ "$code" = "200" ]; then
    q=$(printf '%s' "$body" | grep -o '"queue_depth":[0-9]*' | cut -d: -f2)
    d=$(printf '%s' "$body" | grep -o '"dlq_size":[0-9]*' | cut -d: -f2)
    e=$(printf '%s' "$body" | grep -o '"expired":[0-9]*' | cut -d: -f2)
    ok "readyz: queue=${q:-?} dlq=${d:-?} expired=${e:-?}"
    if [ "${d:-0}" -gt 0 ]; then
        warn "hay $d mensaje(s) en DLQ: /admin/messages?status=dead (replay: POST /admin/messages/{id}/replay)"
    fi
else
    bad "readyz $code: $body"
fi

# 3) Postgres dev (best-effort: el hub puede correr contra otro DSN)
if docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^ravenhub-postgres-1$'; then
    ok "postgres dev corriendo (host :${POSTGRES_PORT:-5435})"
else
    warn "postgres dev no está corriendo (make dev)"
fi

# 4) Backup reciente y cifrado (un backup no probado no es un backup)
latest=$(ls -t backups/ravenhub-* 2>/dev/null | head -n1 || true)
if [ -z "$latest" ]; then
    warn "sin backups en backups/ (make backup)"
else
    case "$latest" in
        *.age) ok "último backup cifrado: $(basename "$latest")" ;;
        *.sql.gz) warn "último backup SIN cifrar: $(basename "$latest")" ;;
    esac
    age_days=$(( ( $(date +%s) - $(stat -c %Y "$latest") ) / 86400 ))
    if [ "$age_days" -gt 7 ]; then
        warn "backup con $age_days días (objetivo: ≤7) — make backup && make backup-verify"
    else
        ok "backup reciente (${age_days}d)"
    fi
fi

# 5) Rotación de logs Docker (los errores/canal log pueden contener PII)
if docker inspect ravenhub-ravenhub-1 >/dev/null 2>&1; then
    logopts=$(docker inspect ravenhub-ravenhub-1 --format '{{json .HostConfig.LogConfig.Config}}' 2>/dev/null || true)
    case "$logopts" in
        *max-size*) ok "logs Docker con límite de tamaño" ;;
        *) warn "logs Docker sin max-size/max-file (crecen sin límite)" ;;
    esac
fi

echo
if [ "$FAIL" = "0" ]; then echo "✓ status: hub operativo"; else echo "✗ status: revisa lo anterior"; fi
exit "$FAIL"
