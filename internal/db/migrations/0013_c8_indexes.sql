-- 0013: índices de las consultas que crecen con el historial.
--
-- - /readyz cuenta 'expired' y /admin/stats cuenta estados: sin índice, seq
--   scan sobre 30d de mensajes.
-- - El listado scoped usa payload->'to' ?| text[]: GIN jsonb_path_ops.
-- - La auditoría se filtra por proyecto y se barre por created_at.
-- - deliveries.destination_id se usa para el FK ON DELETE SET NULL.

CREATE INDEX IF NOT EXISTS idx_messages_status
    ON messages(status);
CREATE INDEX IF NOT EXISTS idx_messages_expired
    ON messages(updated_at) WHERE status = 'expired';
CREATE INDEX IF NOT EXISTS idx_messages_pending_due
    ON messages(next_attempt_at) WHERE status IN ('queued', 'delivering');
CREATE INDEX IF NOT EXISTS idx_messages_payload_to
    ON messages USING gin ((payload -> 'to') jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_audit_project_created
    ON audit_log(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_created
    ON audit_log(created_at);
CREATE INDEX IF NOT EXISTS idx_deliveries_destination
    ON deliveries(destination_id);
