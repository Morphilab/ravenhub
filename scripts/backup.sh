#!/bin/sh
# Backup cifrado de la DB de RavenHub (: un backup no probado no es un backup).
# Uso: RAVENHUB_DB_CONTAINER=... AGE_PUBLIC_KEY=age1... ./backup.sh
# Fail-closed: sin AGE_PUBLIC_KEY NO se escribe un dump en claro. Para un dev
# explícito donde no haya age: BACKUP_ALLOW_PLAINTEXT=1 ./backup.sh
set -euo pipefail
umask 077 # dumps con secretos de canales y contenido de mensajes: 0600

CONTAINER="${RAVENHUB_DB_CONTAINER:-ravenhub-postgres-1}"
DBUSER="${POSTGRES_USER:-ravenhub}"
DBNAME="${POSTGRES_DB:-ravenhub}"
OUTDIR="${BACKUP_DIR:-backups}"
STAMP="$(date +%Y%m%d-%H%M%S)"

# config: entorno > .env > defaults. AGE_PUBLIC_KEY es la clave PÚBLICA (puede
# vivir en .env sin riesgo); la identidad PRIVADA jamás va a un archivo del repo
# (por defecto: ~/.config/ravenhub-backup/age-identity.txt, permisos 600).
if [ -f .env ]; then
	POSTGRES_USER="${POSTGRES_USER:-$(sed -n 's/^POSTGRES_USER=//p' .env | tail -n1 | tr -d '"')}"
	POSTGRES_DB="${POSTGRES_DB:-$(sed -n 's/^POSTGRES_DB=//p' .env | tail -n1 | tr -d '"')}"
	AGE_PUBLIC_KEY="${AGE_PUBLIC_KEY:-$(sed -n 's/^AGE_PUBLIC_KEY=//p' .env | tail -n1 | tr -d '"')}"
	DBUSER="${POSTGRES_USER:-ravenhub}"
	DBNAME="${POSTGRES_DB:-ravenhub}"
fi

# age instalado como binario de usuario si no está en el PATH
AGE_BIN="$(command -v age || true)"
[ -x "$AGE_BIN" ] || AGE_BIN="$HOME/.local/bin/age"

mkdir -p "$OUTDIR"

ENCRYPT=0
if [ -n "${AGE_PUBLIC_KEY:-}" ]; then
    ENCRYPT=1
elif [ "${BACKUP_ALLOW_PLAINTEXT:-0}" != "1" ]; then
    echo "✗ AGE_PUBLIC_KEY sin definir (ni entorno ni .env): backup en claro BLOQUEADO." >&2
    echo "  Define AGE_PUBLIC_KEY o, solo en dev, usa BACKUP_ALLOW_PLAINTEXT=1." >&2
    exit 1
else
    echo "⚠  AGE_PUBLIC_KEY sin definir y BACKUP_ALLOW_PLAINTEXT=1: el dump queda SIN CIFRAR" >&2
fi

FILE="$OUTDIR/ravenhub-$STAMP.sql.gz"
DONE=0
cleanup() {
    # ni un dump parcial ni el plaintext intermedio pueden quedar en backups/
    if [ "$DONE" != "1" ] && [ -n "$FILE" ] && [ -f "$FILE" ]; then
        rm -f "$FILE"
    fi
    if [ "$ENCRYPT" = "1" ] && [ -n "$FILE" ] && [ -f "$FILE" ]; then
        rm -f "$FILE"
    fi
}
trap cleanup EXIT

echo "· dumpeando $DBNAME desde $CONTAINER…"
docker exec "$CONTAINER" pg_dump -U "$DBUSER" "$DBNAME" | gzip > "$FILE"

if [ "$ENCRYPT" = "1" ]; then
    if ! "$AGE_BIN" -r "$AGE_PUBLIC_KEY" -o "$FILE.age" "$FILE"; then
        echo "✗ falló el cifrado age; se elimina el dump en claro" >&2
        exit 1
    fi
    rm -f "$FILE"
    FILE="$FILE.age"
fi
DONE=1
trap - EXIT
echo "✓ backup: $FILE"
echo "  Verifícalo: make backup-verify (restaura en un Postgres efímero y compara)"
