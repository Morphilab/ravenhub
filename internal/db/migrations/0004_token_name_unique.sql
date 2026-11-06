-- 0004: un solo token activo por (proyecto, nombre). El historial (revocados)
-- se conserva: RotateToken crea un gemelo y revoca el viejo.
-- Postgres trata NULL como distinto: índice separado para admin (project_id NULL).
CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_active_name
    ON tokens(project_id, name) WHERE project_id IS NOT NULL AND revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_active_admin_name
    ON tokens(name) WHERE project_id IS NULL AND revoked_at IS NULL;
