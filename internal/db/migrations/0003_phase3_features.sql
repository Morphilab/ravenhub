-- Antispam y cobertura.
-- Compatible con 0001 y 0002. Sin adjuntos: los difiere
-- ("los agentes pasan enlaces"). Sin canales nuevos.

-- 1) send_at: envío programado. El worker solo reclama
--    messages con next_attempt_at <= now(), así que basta con programar
--    el primer intento; el TTL normal lo acota (no se puede programar
--    más allá de la ventana de vida).
ALTER TABLE messages ADD COLUMN IF NOT EXISTS send_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_messages_send_at
    ON messages(send_at) WHERE status = 'queued' AND send_at IS NOT NULL;

-- 2) Agregación anti-spam: N mensajes con mismo título+tags en
--    una ventana → un digest ("⚠️ 47 errores iguales en 10 min: <último>").
ALTER TABLE messages ADD COLUMN IF NOT EXISTS aggregation_key TEXT;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS aggregated_count INT NOT NULL DEFAULT 1;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS is_digest BOOLEAN NOT NULL DEFAULT FALSE;

-- Búsqueda del digest abierto de un proyecto+clave (ventana abierta =
-- aún en cola y con next_attempt_at futuro: sigue recolectando).
CREATE INDEX IF NOT EXISTS idx_messages_open_digest
    ON messages(project_id, aggregation_key, next_attempt_at)
    WHERE is_digest AND status = 'queued';

-- 3) Reglas de enrutamiento por tags/prioridad (:
--    `priority=urgent → oncall`). Config de admin, evaluadas en intake
--    solo para añadir destinos: el consumidor nunca decide el canal.
CREATE TABLE IF NOT EXISTS routing_rules (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    match_tags      TEXT[] NOT NULL DEFAULT '{}',   -- todos deben estar (AND)
    match_priority  TEXT,                            -- prioridad mínima, opcional
    match_payload   JSONB NOT NULL DEFAULT '{}',     -- subconjunto sobre data
    destinations    TEXT[] NOT NULL,                 -- destinos/grupos a añadir
    stop_processing BOOLEAN NOT NULL DEFAULT FALSE,  -- no evaluar más reglas
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    priority        INT NOT NULL DEFAULT 100,        -- orden: menor = primero
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_routing_rules_project
    ON routing_rules(project_id, priority, created_at);
