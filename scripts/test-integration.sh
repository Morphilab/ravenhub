#!/bin/bash
# Tests de integración de RavenHub: levanta un Postgres efímero en Docker,
# corre la suite e2e (criterios de aceptación) y lo destruye.
set -euo pipefail

CONTAINER=ravenhub-test-pg
PORT=5434
export RAVENHUB_TEST_DSN="postgres://ravenhub:ravenhub-dev@localhost:${PORT}/ravenhub?sslmode=disable"
RUN='TestNotifyE2E|TestIdempotency|TestRevocation|TestCrossTenant|TestScopes|TestBackpressure|TestProjectHardLimit|TestBurst|TestTemplate|TestQuietHours|TestInputValidation|TestRetention|TestCrashRecovery|TestReplayDLQ|TestAdminTestDestination|TestSyncTrue|TestSendAtSchedules|TestRoutingRulesAppend|TestAggregationDigest|TestDigestIdempotency|TestWebhookDelivery|TestDeleteProject|TestRotateTokenFlow|TestScopesMaxPriorityNormalized|TestSyncDuplicateRespectsScopes|TestListMessagesScopedSeesOwn|TestExpiredLeavesTrace|TestTTLBudgetGuardGoesToDLQ|TestTruncatedDeliveryFlagged|TestShedMetric|TestSeedProvisioningEndToEnd|TestAdminProjectOptionsAndGroupDelete|TestGroupFanOut|TestEmptyGroup|TestAuthThrottle|TestFoldedKeyDedup|TestDeleteAccountAuditsCascade|TestDeleteDestination|TestTelegramStub|TestDiscordStub|TestReEncrypt|TestDigestFoldSkipsClaimedDigest|TestExpireSkipsLiveDelivering|TestProjectQuotaIgnoresFarScheduled'

cleanup() {
	docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
echo "· levantando Postgres efímero en :${PORT}…"
docker run -d --name "$CONTAINER" -e POSTGRES_USER=ravenhub -e POSTGRES_PASSWORD=ravenhub-dev \
	-e POSTGRES_DB=ravenhub -p "127.0.0.1:${PORT}:5432" postgres:16-alpine >/dev/null

echo "· esperando a que Postgres esté listo…"
ready=0
for i in $(seq 1 80); do
	# pg_isready puede responder durante el arranque temporal de initdb; la
	# consulta real evita que las migraciones corran contra un server que
	# todavía se está reiniciando (timeouts intermitentes).
	if docker exec "$CONTAINER" pg_isready -U ravenhub >/dev/null 2>&1 &&
	   docker exec "$CONTAINER" psql -U ravenhub -d ravenhub -tAc 'select 1' >/dev/null 2>&1; then
		ready=1; break
	fi
	sleep 0.5
done
if [ "$ready" != "1" ]; then
	echo "✗ el Postgres efímero no estuvo listo a tiempo — abortando"
	exit 1
fi

# Canario directo: cada patrón del -run debe matchear ≥1 test real. Un rename
# desacoplado sacaría el test de la suite EN SILENCIO (go test -run con un
# nombre inexistente sale 0); esto lo convierte en fallo ruidoso.
echo "· canario: cada patrón del -run debe matchear un test real…"
IFS='|' read -r -a patterns <<< "$RUN"
for p in "${patterns[@]}"; do
	n=$(go test -list "^${p}" ./internal/e2e 2>/dev/null | grep -c '^Test' || true)
	if [ "${n:-0}" -eq 0 ]; then
		echo "✗ el patrón '${p}' no matchea ningún test (¿rename?) — actualiza scripts/test-integration.sh"
		exit 1
	fi
done

# Canario inverso: todo test e2e debe estar cubierto por algún patrón. Sin
# esto, un test nuevo (o un patrón olvidado) queda fuera de la suite y el
# resumen sigue verde.
echo "· canario inverso: todo test e2e debe estar en la suite…"
mapfile -t all_tests < <(go test -list '^Test' ./internal/e2e 2>/dev/null | grep '^Test' || true)
missing=""
for t in "${all_tests[@]}"; do
	hit=0
	for p in "${patterns[@]}"; do
		[[ "$t" == "$p"* ]] && { hit=1; break; }
	done
	[ "$hit" -eq 0 ] && missing="$missing $t"
done
if [ -n "$missing" ]; then
	echo "✗ tests e2e fuera de RUN:$missing — actualiza scripts/test-integration.sh"
	exit 1
fi

echo "· ejecutando suite de integración…"
go test -count=1 -timeout 300s ./internal/... -run "$RUN"
echo "✓ integración verde"
