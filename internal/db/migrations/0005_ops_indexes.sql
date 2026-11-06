-- 0005: índices para los barridos del mantenedor (cada 15 s) y de retención.
-- Antes: reaper, expirer, gauge de DLQ y borrado de terminales seq-scanneaban
-- TODO el historial de messages en cada tick — con retención de 30d la tabla
-- vive en su tope y los tres scans por tick degradaban la DB y la cola.
CREATE INDEX IF NOT EXISTS idx_messages_reaper
    ON messages(updated_at) WHERE status = 'delivering';
CREATE INDEX IF NOT EXISTS idx_messages_expires
    ON messages(expires_at) WHERE status IN ('queued', 'delivering');
-- gauge de dlq_size y conteos de estado: partial index-only scans.
CREATE INDEX IF NOT EXISTS idx_messages_dead
    ON messages(updated_at) WHERE status = 'dead';
CREATE INDEX IF NOT EXISTS idx_messages_terminal_updated
    ON messages(updated_at) WHERE status IN ('sent', 'failed', 'dead', 'expired');
