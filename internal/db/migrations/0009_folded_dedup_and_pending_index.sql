-- 0009: índices para el dedup de folded_keys y el pending de la cola.
--
-- 1) El dedup de mensajes plegados a un digest consulta folded_keys también
--    TRAS el cierre de la ventana (vida de fila): sin índice, ese lookup
--    escaneaba el historial completo del proyecto en cada notify con clave.
-- 2) El shedding por proyecto cuenta queued+delivering en CADA intake
--    (CountQueuedForProject): el índice parcial evita que ese costo crezca
--    con los 30 días de retención de terminales en la misma tabla.
CREATE INDEX IF NOT EXISTS idx_messages_folded_keys
    ON messages USING gin (folded_keys) WHERE is_digest;
CREATE INDEX IF NOT EXISTS idx_messages_project_pending
    ON messages (project_id) WHERE status IN ('queued', 'delivering');
