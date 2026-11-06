#!/bin/bash
# Smoke test end-to-end de RavenHub contra un hub corriendo en $RAVENHUB_URL.
# Ejercicio el criterio "un agente notifica con una llamada y llega" usando
# SOLO la API pública: curl es el agente. Canal log = sin credenciales reales.
# Sin -e a propósito: los fallos se cuentan (✓/✗) y el resumen decide el exit.
#
# Los tokens viajan como config de curl leída de un FD (-K <(...)), jamás en
# argv: los argumentos de cualquier proceso son legibles por el resto de
# usuarios locales vía `ps`. printf es builtin de bash: tampoco deja rastro.
set -uo pipefail

RAVENHUB_URL="${RAVENHUB_URL:-http://localhost:8085}"

# RAVENHUB_ADMIN_TOKEN: del entorno o de .env — sin `set -a`: exportar el
# .env completo pondría la master key en el entorno de todos los hijos.
ADMIN="${RAVENHUB_ADMIN_TOKEN:-}"
if [ -z "$ADMIN" ] && [ -f .env ]; then
    ADMIN=$(sed -n 's/^RAVENHUB_ADMIN_TOKEN=//p' .env | sed 's/^"//;s/"$//' | tail -n1)
fi
if [ -z "$ADMIN" ]; then
    echo "✗ falta RAVENHUB_ADMIN_TOKEN (mira el log de arranque si se autogeneró en dev)"; exit 1
fi

# hdrs TOKEN [IDEMPOTENCY_KEY] emite la config de curl por stdout
hdrs() {
    printf 'header = "Authorization: Bearer %s"\nheader = "Content-Type: application/json"\n' "$1"
    [ -n "${2:-}" ] && printf 'header = "Idempotency-Key: %s"\n' "$2"
    true
}

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL+1)); echo "  ✗ $1"; }
check() { # desc esperado real
    if [ "$2" = "$3" ]; then ok "$1"; else fail "$1 (esperaba $2, got $3)"; fi
}

echo "· smoke contra $RAVENHUB_URL"

# 1. liveness
code=$(curl -s -o /dev/null -w '%{http_code}' "$RAVENHUB_URL/healthz")
check "healthz responde" 200 "$code"

# 2. proyecto efímero + token (admin)
PROJ="smoke-$(date +%s)"
CREATE=$(curl -s -K <(hdrs "$ADMIN") -X POST "$RAVENHUB_URL/admin/projects" \
    -d "{\"name\":\"$PROJ\",\"token_name\":\"smoke-agent\"}")
PROJ_ID=$(echo "$CREATE" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
TOKEN=$(echo "$CREATE" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
[ -n "$PROJ_ID" ] && [ -n "$TOKEN" ] && ok "proyecto+token creados" || { fail "crear proyecto (respuesta sin token)"; exit 1; }

# 3. cuenta log + destino (canal dry-run, sin credenciales reales)
ACCT=$(curl -s -K <(hdrs "$ADMIN") -X POST "$RAVENHUB_URL/admin/projects/$PROJ_ID/accounts" \
    -d '{"channel":"log","label":"log-smoke","creds":{}}')
ACCT_ID=$(echo "$ACCT" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$ADMIN") -X POST "$RAVENHUB_URL/admin/projects/$PROJ_ID/destinations" \
    -d "{\"name\":\"log:smoke\",\"account_id\":\"$ACCT_ID\",\"target\":{}}")
check "destino log creado" 201 "$code"

# 4. EL AGENTE NOTIFICA CON UNA LLAMADA (sync=true: espera entrega real)
RESULT=$(curl -s -K <(hdrs "$TOKEN" "smoke-$(date +%s)") -X POST "$RAVENHUB_URL/v1/notify?sync=true" \
    -d '{"to":["log:smoke"],"title":"Smoke test","message":"un agente notifica con una llamada","priority":"high"}')
STATUS=$(echo "$RESULT" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
check "notificación entregada (sync)" "sent" "$STATUS"

# 5. idempotencia: misma clave → mismo id
KEY="smoke-idem-$(date +%s)"
ID1=$(curl -s -K <(hdrs "$TOKEN" "$KEY") -X POST "$RAVENHUB_URL/v1/notify" \
    -d '{"to":["log:smoke"],"title":"i","message":"m"}' | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
ID2=$(curl -s -K <(hdrs "$TOKEN" "$KEY") -X POST "$RAVENHUB_URL/v1/notify" \
    -d '{"to":["log:smoke"],"title":"i","message":"m"}' | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
[ -n "$ID1" ] && [ "$ID1" = "$ID2" ] && ok "idempotencia (mismo id)" || fail "idempotencia ($ID1 vs $ID2)"

# 6. destino desconocido → 400
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$TOKEN") -X POST "$RAVENHUB_URL/v1/notify" \
    -d '{"to":["no:existe"],"title":"t","message":"m"}')
check "destino desconocido → 400" 400 "$code"

# 7. token falso → 401
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "rvn_falso_000") -X POST "$RAVENHUB_URL/v1/notify" \
    -d '{"to":["log:smoke"],"title":"t","message":"m"}')
check "token falso → 401" 401 "$code"

# 8. scopes: token acotado no puede urgent
SCOPED=$(curl -s -K <(hdrs "$ADMIN") -X POST "$RAVENHUB_URL/admin/projects/$PROJ_ID/tokens" \
    -d '{"name":"scop","max_priority":"low"}' | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$SCOPED") -X POST "$RAVENHUB_URL/v1/notify" \
    -d '{"to":["log:smoke"],"title":"t","message":"m","priority":"urgent"}')
check "scope de prioridad → 403" 403 "$code"

# 9. revocación instantánea: crear un token, revocarlo por su id y verificar
#    con su propio token.
ROT_resp=$(curl -s -K <(hdrs "$ADMIN") -X POST "$RAVENHUB_URL/admin/projects/$PROJ_ID/tokens" \
    -d '{"name":"revocable"}')
ROT_ID=$(echo "$ROT_resp" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
ROTTOKEN=$(echo "$ROT_resp" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
if [ -z "$ROT_ID" ] || [ -z "$ROTTOKEN" ]; then
    fail "crear token revocable (respuesta sin token)"
else
    curl -s -K <(hdrs "$ADMIN") -X POST "$RAVENHUB_URL/admin/tokens/$ROT_ID/revoke" >/dev/null
    code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$ROTTOKEN") -X POST "$RAVENHUB_URL/v1/notify" \
        -d '{"to":["log:smoke"],"title":"t","message":"m"}')
    check "token revocado → 401 al instante" 401 "$code"
fi

# 10. truncado con rastro: mensaje > 8000 en el canal log → deliveries.truncated
# (antes de la ráfaga: el rate-limit del proyecto se agota con ella)
LONG=$(python3 -c "print('x'*8100)" 2>/dev/null || printf 'x%.0s' $(seq 1 8100))
TRUNC=$(curl -s -K <(hdrs "$TOKEN" "smoke-trunc-$(date +%s)") -X POST "$RAVENHUB_URL/v1/notify?sync=true" \
    -d "{\"to\":[\"log:smoke\"],\"title\":\"truncado\",\"message\":\"$LONG\",\"priority\":\"high\"}")
if printf '%s' "$TRUNC" | grep -q '"truncated":true'; then
    ok "truncado marcado (deliveries.truncated)"
else
    fail "truncado sin marcar: $(printf '%s' "$TRUNC" | cut -c1-200)"
fi

# 11. ráfaga de 50: el intake rate-limiter (60/min, burst 10 por defecto)
# debe responder SIEMPRE con 202 (aceptado) o 429 (shedding) — nunca 5xx ni
# cuelgues. El drenaje completo de ráfagas lo cubre TestBurst1000.
OK=0
for i in $(seq 1 50); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$TOKEN") -X POST "$RAVENHUB_URL/v1/notify" \
        -d "{\"to\":[\"log:smoke\"],\"title\":\"ráfaga\",\"message\":\"$i\"}") || code=0
    case "$code" in
        202|429) OK=$((OK+1)) ;;
    esac
done
check "ráfaga 50/50 respondidas (202 ok / 429 shedding)" 50 "$OK"

# 12. options por admin API: PATCH válido → 200; typo → 400 (fail-fast)
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$ADMIN") -X PATCH "$RAVENHUB_URL/admin/projects/$PROJ_ID" \
    -d '{"options":{"ttl":{"urgent":"10m"}}}')
check "PATCH options válido → 200" 200 "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$ADMIN") -X PATCH "$RAVENHUB_URL/admin/projects/$PROJ_ID" \
    -d '{"options":{"ttl":{"urgent":"abc"}}}')
check "PATCH options inválido → 400" 400 "$code"

# 14. autolimpieza: el proyecto efímero no se acumula en la DB.
code=$(curl -s -o /dev/null -w '%{http_code}' -K <(hdrs "$ADMIN") -X DELETE "$RAVENHUB_URL/admin/projects/$PROJ_ID")
check "proyecto smoke borrado (autolimpieza)" 200 "$code"

echo
echo "════════ SMOKE: $PASS ✓ / $FAIL ✗ ════════"
[ "$FAIL" = "0" ]
