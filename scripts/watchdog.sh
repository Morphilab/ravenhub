#!/bin/sh
# Watchdog de RavenHub: ¿quién notifica que el notificador murió?
# Corre FUERA del pipeline (compose.prod, bot bootstrap propio). Si /healthz
# falla 2 veces seguidas o la DLQ supera el umbral, avisa directo a Telegram.
# Solo usa busybox (wget/awk/sh): funciona en la imagen alpine del compose
# sin instalar nada.
set -euo pipefail

HEALTHZ="${WATCHDOG_HEALTHZ_URL:-http://ravenhub:8080/healthz}"
READYZ="${WATCHDOG_READYZ_URL:-http://ravenhub:8080/readyz}"
METRICS="${WATCHDOG_METRICS_URL:-http://ravenhub:8080/metrics}"
BOT="${WATCHDOG_BOT_TOKEN:-}"
CHAT="${WATCHDOG_CHAT_ID:-}"
INTERVAL="${WATCHDOG_INTERVAL:-300}"
DLQ_THRESHOLD="${WATCHDOG_DLQ_THRESHOLD:-50}"
# INTERVAL debe ser un entero de segundos: un valor basura haría fallar el
# `sleep` y, con set -e, el restart:always convertiría el watchdog en un
# crash-loop que nunca alerta.
case "$INTERVAL" in
    ''|*[!0-9]*) INTERVAL=300 ;;
esac

if [ -z "$BOT" ] || [ -z "$CHAT" ]; then
    echo "watchdog: WATCHDOG_BOT_TOKEN/WATCHDOG_CHAT_ID sin configurar — las alertas solo aparecerán en este log"
fi

fails=0
alert() { # envía directo a Telegram (JSON), sin pasar por el Hub
    if [ -z "$BOT" ] || [ -z "$CHAT" ]; then
        echo "watchdog: $1 (sin bot configurado)"
        return
    fi
    # CHAT solo puede ser id numérico (incl. -100...): evita inyección JSON
    case "$CHAT" in
        ''|*[!0-9-]*) echo "watchdog: CHAT inválido (solo dígitos y -)"; return ;;
    esac
    # Nota: el BOT va en la URL (API de Telegram lo exige) y es visible en
    # `ps` local — riesgo aceptado: el watchdog corre en el host/VPS del operador.
    wget -q -T 10 -O /dev/null \
        --header='Content-Type: application/json' \
        --post-data="{\"chat_id\":\"$CHAT\",\"text\":\"$1\"}" \
        "https://api.telegram.org/bot${BOT}/sendMessage" \
        || echo "watchdog: no se pudo enviar la alerta a Telegram"
}

# dedup de la alerta de DLQ: sin memoria, la alerta se repetiría en CADA ciclo
# mientras la DLQ superara el umbral (spam de Telegram cada INTERVAL). Se
# alerta al ENTRAR en estado "por encima" y de nuevo solo tras volver a bajar.
dlq_alerted=0

while :; do
    # liveness (healthz) + readiness (readyz: postgres + worker). El readyz
    # detecta DB caída/worker parado que healthz no ve.
    if wget -q -T 10 -O /dev/null "$HEALTHZ" 2>/dev/null && wget -q -T 10 -O /dev/null "$READYZ" 2>/dev/null; then
        if [ "$fails" -ge 2 ]; then
            alert "RavenHub watchdog: recuperado: readyz vuelve a responder tras $fails fallos"
        fi
        fails=0
        # DLQ crecida = algo está fallando en silencio (serie gauge de /metrics)
        # awk numérico: tolera enteros, decimales y notación científica (1e+06).
        # `|| true`: un blip de /metrics no puede matar el script (set -e +
        # pipefail); healthz/readyz ya están en un if, inmunes.
        dlq=$(wget -q -T 10 -O - "$METRICS" 2>/dev/null | awk '/^ravenhub_dlq_size /{v=$2} END{printf "%d", v+0}' || true)
        case "${dlq:-0}" in
            ''|*[!0-9.]*) dlq=0 ;;
        esac
        over=$(printf '%s %s\n' "$dlq" "$DLQ_THRESHOLD" | awk '{print ($1 > $2) ? 1 : 0}')
        if [ "$over" = "1" ] && [ "$dlq_alerted" != "1" ]; then
            dlq_alerted=1
            alert "🚨 RavenHub watchdog: DLQ con $dlq mensajes (umbral $DLQ_THRESHOLD): revisa /admin/messages?status=dead"
        elif [ "$over" = "0" ]; then
            dlq_alerted=0
        fi
    else
        fails=$((fails + 1))
        echo "watchdog: fallo $fails en healthz/readyz"
        [ "$fails" -eq 2 ] && alert "RavenHub watchdog: healthz/readyz sin respuesta 2 veces seguidas — el notificador está caído"
    fi
    sleep "$INTERVAL"
done
