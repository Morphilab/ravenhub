-- RavenHub esquema inicial
-- Postgres como único store: config, cola, logs, auditoría.

CREATE TABLE IF NOT EXISTS projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    options     JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Tokens: solo hash SHA-256 + prefijo visible. project_id NULL = token admin.
CREATE TABLE IF NOT EXISTS tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    hash        TEXT NOT NULL UNIQUE,
    prefix      TEXT NOT NULL,
    scopes      JSONB NOT NULL DEFAULT '{}',
    expires_at  TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tokens_project ON tokens(project_id);

-- Cuentas: credencial concreta por canal, cifrada en reposo (AES-256-GCM).
CREATE TABLE IF NOT EXISTS accounts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    channel          TEXT NOT NULL,
    label            TEXT NOT NULL,
    creds_encrypted  BYTEA NOT NULL,
    limits           JSONB NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, label)
);
CREATE INDEX IF NOT EXISTS idx_accounts_project ON accounts(project_id);

-- Destinos: cuenta + destinatario concreto, con nombre único por proyecto.
CREATE TABLE IF NOT EXISTS destinations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    target       JSONB NOT NULL,
    quiet_hours  JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_destinations_project ON destinations(project_id);

CREATE TABLE IF NOT EXISTS destination_groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS destination_group_members (
    group_id       UUID NOT NULL REFERENCES destination_groups(id) ON DELETE CASCADE,
    destination_id UUID NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, destination_id)
);

CREATE TABLE IF NOT EXISTS templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    body        TEXT NOT NULL,
    overrides   JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

-- Mensajes = la cola. El worker reclama con FOR UPDATE SKIP LOCKED.
CREATE TABLE IF NOT EXISTS messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    token_id        UUID REFERENCES tokens(id) ON DELETE SET NULL,
    payload         JSONB NOT NULL,
    priority        TEXT NOT NULL DEFAULT 'normal',
    status          TEXT NOT NULL DEFAULT 'queued',
    dedup_key       TEXT,
    expires_at      TIMESTAMPTZ NOT NULL,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Idempotencia: (proyecto, clave) único en ventana de vida del mensaje.
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_dedup
    ON messages(project_id, dedup_key) WHERE dedup_key IS NOT NULL;
-- Reclamo del worker (índice parcial = scan barato incluso con historial).
CREATE INDEX IF NOT EXISTS idx_messages_claim
    ON messages(next_attempt_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS idx_messages_project_created
    ON messages(project_id, created_at DESC);

-- Entregas por destino, con error y id del proveedor para dedupe post-hoc.
CREATE TABLE IF NOT EXISTS deliveries (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id       UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    destination_id   UUID,
    dest_name        TEXT NOT NULL,
    channel          TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    error            TEXT,
    provider_msg_id  TEXT,
    latency_ms       INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_id, dest_name)
);
CREATE INDEX IF NOT EXISTS idx_deliveries_message ON deliveries(message_id);

-- Auditoría append-only: un trigger bloquea UPDATE/DELETE.
CREATE TABLE IF NOT EXISTS audit_log (
    id           BIGSERIAL PRIMARY KEY,
    project_id   UUID,
    token_prefix TEXT,
    action       TEXT NOT NULL,
    subject      TEXT,
    detail       JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log es append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_log_no_update ON audit_log;
CREATE TRIGGER audit_log_no_update
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
