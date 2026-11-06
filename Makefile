# RavenHub — comandos del ciclo de desarrollo (local-first)
# `make dev` es el camino habitual: Postgres en Docker + Hub con `go run`.

GO      ?= go
# El DSN sale de .env (password rotada incluida); el fallback solo aplica si
# no hay .env. Antes el default hardcodeado rompía make seed/migrate tras
# rotar la password de Postgres.
DSN     ?= $(shell sed -n 's/^RAVENHUB_DSN=//p' .env 2>/dev/null | tail -n1 | tr -d '"')
ifeq ($(strip $(DSN)),)
DSN := postgres://ravenhub:ravenhub-dev@localhost:5435/ravenhub
endif
# El DSN (con el password de Postgres) viaja por ENTORNO de las recetas, nunca
# por argv: `RAVENHUB_DSN=$(DSN) go run ...` dejaba el password legible en
# /proc/*/cmdline durante toda la invocación. Los binarios también leen .env
# por sí mismos; el export cubre el fallback sin .env.
export RAVENHUB_DSN := $(DSN)
SEED    ?= dev/seed.yaml
COMPOSE := docker compose -f compose.dev.yaml
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X ravenhub/internal/cli.Version=$(VERSION)

.PHONY: help build run dev up full down logs migrate seed test test-race test-sdk test-integration smoke status watchdog fmt vet lint staticcheck backup backup-verify docker-build clean

help: ## lista los targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## compila los binarios (ravenhub + ravenctl)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ravenhub ./cmd/ravenhub
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ravenctl ./cmd/ravenctl

run: ## ejecuta el Hub en local (requiere postgres arriba)
	$(GO) run ./cmd/ravenhub serve

dev: up ## entorno dev completo: postgres en docker + `make run` manual
	@echo "▶ Postgres arriba. Ejecuta en otra terminal:  make run"
	@echo "  (o todo-en-docker: make full)"

up: ## levanta postgres (y .env si falta; genera master key/admin token si faltan)
	@test -f .env || (cp example.env .env && chmod 600 .env && echo "· .env creado desde example.env (permisos 600)")
	@if [ -z "$$(sed -n 's/^RAVENHUB_MASTER_KEY=//p' .env | tail -n1 | tr -d '\"' | tr -d '[:space:]')" ]; then \
		key=$$(openssl rand -hex 32); \
		test -n "$$key" || { echo "✗ no se pudo generar RAVENHUB_MASTER_KEY (¿openssl?)"; exit 1; }; \
		sed -i "s|^RAVENHUB_MASTER_KEY=.*|RAVENHUB_MASTER_KEY=$$key|" .env; \
		chmod 600 .env; \
		echo "· RAVENHUB_MASTER_KEY generada en .env (no se imprime)"; \
	fi
	@if [ -z "$$(sed -n 's/^RAVENHUB_ADMIN_TOKEN=//p' .env | tail -n1 | tr -d '\"' | tr -d '[:space:]')" ]; then \
		tok="rva_admin_$$(openssl rand -hex 16)"; \
		test -n "$$tok" || { echo "✗ no se pudo generar RAVENHUB_ADMIN_TOKEN (¿openssl?)"; exit 1; }; \
		sed -i "s|^RAVENHUB_ADMIN_TOKEN=.*|RAVENHUB_ADMIN_TOKEN=$$tok|" .env; \
		chmod 600 .env; \
		echo "· RAVENHUB_ADMIN_TOKEN generado en .env (no se imprime)"; \
	fi
	$(COMPOSE) up -d postgres
	@until $(COMPOSE) exec -T postgres pg_isready -U ravenhub >/dev/null 2>&1; do sleep 0.5; done
	@echo "· Postgres listo (host port $${POSTGRES_PORT:-5435})"

full: ## todo-en-docker (hub incluido, sin hot-reload)
	RAVENHUB_VERSION=$(VERSION) $(COMPOSE) --profile full up -d --build
	@echo "· Hub en http://localhost:8085"

down: ## para el entorno dev
	$(COMPOSE) down

logs: ## logs del compose
	$(COMPOSE) logs -f

migrate: ## aplica migraciones
	@$(GO) run ./cmd/ravenhub migrate

seed: ## aplica el seed declarativo (SEED=dev/seed.yaml; si falta, demo log-only)
	@test -f $(SEED) || (mkdir -p dev && cp seed.demo.yaml $(SEED) && echo "· seed DEMO copiado a $(SEED) (solo log; para canales reales usa seed.example.yaml)")
	@$(GO) run ./cmd/ravenhub import $(SEED)

fmt: ## formatea
	$(GO) fmt ./...

vet: ## go vet
	$(GO) vet ./...

staticcheck: ## staticcheck (clases de bugs que vet no ve; versión pinneada)
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

lint: fmt vet staticcheck ## fmt + vet + staticcheck

test: ## tests unitarios (sin DB; la suite e2e se SALTA sin DSN — verde aquí no la cubre)
	@echo "· ojo: internal/e2e hace t.Skip sin RAVENHUB_TEST_DSN — usa make test-integration para la e2e"
	$(GO) test ./...

test-race: ## tests unitarios con el detector de carreras (el "race" de los cierres de ciclo)
	$(GO) test -race ./...

test-sdk: ## tests del SDK Python (sin red: transporte inyectable)
	python3 -m unittest discover -s sdk -p 'test_*.py' -v

test-integration: ## tests de integración (levanta postgres efímero)
	@bash scripts/test-integration.sh

smoke: ## smoke test end-to-end contra un hub corriendo en :8085
	@bash scripts/smoke.sh

status: ## estado del entorno dev (hub, postgres, .env, backup, logs) — el "doctor"
	@bash scripts/status.sh

watchdog: ## watchdog local contra :8085 (sin WATCHDOG_BOT_TOKEN alerta solo al log)
	@WATCHDOG_HEALTHZ_URL="$${WATCHDOG_HEALTHZ_URL:-http://localhost:8085/healthz}" \
	 WATCHDOG_READYZ_URL="$${WATCHDOG_READYZ_URL:-http://localhost:8085/readyz}" \
	 WATCHDOG_METRICS_URL="$${WATCHDOG_METRICS_URL:-http://localhost:8085/metrics}" \
	 WATCHDOG_BOT_TOKEN="$${WATCHDOG_BOT_TOKEN:-$$(sed -n 's/^WATCHDOG_BOT_TOKEN=//p' .env 2>/dev/null | tail -n1)}" \
	 WATCHDOG_CHAT_ID="$${WATCHDOG_CHAT_ID:-$$(sed -n 's/^WATCHDOG_CHAT_ID=//p' .env 2>/dev/null | tail -n1)}" \
	 WATCHDOG_INTERVAL="$${WATCHDOG_INTERVAL:-60}" \
	 sh scripts/watchdog.sh

docker-build: ## construye la imagen
	docker build --build-arg VERSION=$(VERSION) -t ravenhub:local .

backup: ## backup de la DB (cifrado age obligatorio; sin clave falla salvo BACKUP_ALLOW_PLAINTEXT=1)
	@bash scripts/backup.sh

backup-verify: ## restaura el último backup en un Postgres efímero y compara (un backup no probado no es un backup)
	@bash scripts/backup-verify.sh

clean: ## limpia binarios
	rm -rf bin/
