#!/bin/sh
# backup-verify — "un backup no probado no es un backup" (backup.sh).
# Restaura el ÚLTIMO backup en un Postgres efímero (una sola pasada, se
# cuenta cualquier ERROR) y compara conteos con la DB dev viva. Soporta
# dumps .age (descifra con la identidad por defecto) y .sql.gz en claro.
# Uso: make backup-verify   ·   AGE_IDENTITY=/ruta/identidad make backup-verify
set -euo pipefail

CONTAINER=ravenhub-restore-check
LIVE_CONTAINER="${RAVENHUB_DB_CONTAINER:-ravenhub-postgres-1}"
PORT=5437
AGE_IDENTITY="${AGE_IDENTITY:-$HOME/.config/ravenhub-backup/age-identity.txt}"
BACKUP_DIR="${BACKUP_DIR:-backups}"

FILE=$(ls -t "$BACKUP_DIR"/ravenhub-* 2>/dev/null | head -1)
if [ -z "$FILE" ]; then
	echo "✗ no hay backups en $BACKUP_DIR (make backup primero)"
	exit 1
fi
case "$FILE" in
*.age|*.sql.gz) ;;
*) echo "✗ $FILE no es un backup (esperaba .age o .sql.gz)"; exit 1 ;;
esac
echo "· verificando: $FILE"

AGE_BIN="$(command -v age || true)"
[ -x "$AGE_BIN" ] || AGE_BIN="$HOME/.local/bin/age"

dump() {
	case "$FILE" in
	*.age)
		if [ ! -r "$AGE_IDENTITY" ]; then
			echo "✗ dump cifrado con age y sin identidad legible en $AGE_IDENTITY"
			exit 1
		fi
		# el .age envuelve un .sql.gz: descifrar y descomprimir
		"$AGE_BIN" -d -i "$AGE_IDENTITY" "$FILE" | gunzip ;;
	*.sql.gz)
		gunzip -c "$FILE" ;;
	esac
}

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
# Password aleatoria: aunque el bind es loopback, no dejamos una credencial
# conocida sobre un dump restaurado con PII.
RESTORE_PASS="$(openssl rand -hex 16)"
export POSTGRES_PASSWORD="$RESTORE_PASS"
docker run -d --name "$CONTAINER" -e POSTGRES_USER=ravenhub -e POSTGRES_PASSWORD \
	-e POSTGRES_DB=ravenhub -p "127.0.0.1:${PORT}:5432" postgres:16-alpine >/dev/null
unset POSTGRES_PASSWORD
ready=0
for i in $(seq 1 80); do
	if docker exec "$CONTAINER" pg_isready -U ravenhub >/dev/null 2>&1 &&
	   docker exec "$CONTAINER" psql -U ravenhub -d ravenhub -tAc 'select 1' >/dev/null 2>&1; then
		ready=1; break
	fi
	sleep 0.5
done
if [ "$ready" != "1" ]; then
	echo "✗ el Postgres efímero no estuvo listo a tiempo"
	exit 1
fi

echo "· restaurando (una sola pasada, ON_ERROR_STOP)…"
set +e
RESTORE_OUT=$(dump | docker exec -i "$CONTAINER" psql -U ravenhub -d ravenhub -q -v ON_ERROR_STOP=1 2>&1)
RESTORE_RC=$?
set -e
if [ "$RESTORE_RC" != "0" ] || printf '%s' "$RESTORE_OUT" | grep -q '^ERROR'; then
	echo "✗ restauración falló (exit $RESTORE_RC):"
	# sin líneas DETAIL: un "duplicate key" incrusta valores (nombres de
	# proyecto/destino → pseudónimos con posible PII); el código basta
	printf '%s\n' "$RESTORE_OUT" | grep -v '^DETAIL' | head -20
	exit 1
fi

TABLES=$(docker exec "$CONTAINER" psql -U ravenhub -d ravenhub -t -A -c \
	"select count(*) from information_schema.tables where table_schema='public'")
if [ "${TABLES:-0}" -lt 12 ]; then
	echo "✗ solo $TABLES tablas restauradas (esperadas ≥12)"
	exit 1
fi
PROJECTS=$(docker exec "$CONTAINER" psql -U ravenhub -d ravenhub -t -A -c "select count(*) from projects")
if [ "${PROJECTS:-0}" -lt 1 ]; then
	echo "✗ el backup restaurado no tiene ningún proyecto (dump vacío o incompleto)"
	exit 1
fi

echo "· restauración limpia ($TABLES tablas, $PROJECTS proyectos). Conteos restaurada vs viva (Δ = la viva sigue moviéndose; no es fallo):"
for q in "count(*) from projects" "count(*) from accounts" "count(*) from destinations" "count(*) from messages"; do
	R=$(docker exec "$CONTAINER" psql -U ravenhub -d ravenhub -t -A -c "select $q")
	if L=$(docker exec "$LIVE_CONTAINER" psql -U ravenhub -d ravenhub -t -A -c "select $q" 2>/dev/null); then
		if [ "$R" = "$L" ]; then echo "  $q: $R = $L ✓"; else echo "  $q: $R vs $L Δ"; fi
	else
		echo "  $q: $R (viva no accesible)"
	fi
done
echo "✓ backup verificado-restaurable: $FILE"
