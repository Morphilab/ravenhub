-- 0014: FK de audit_log.project_id para cerrar la carrera de inserts
-- concurrentes durante el borrado de un proyecto. En instalaciones
-- nuevas 0011 ya la crea; IF NOT EXISTS mantiene el upgrade idempotente.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'audit_log_project_id_fkey'
    ) THEN
        ALTER TABLE audit_log
            ADD CONSTRAINT audit_log_project_id_fkey
            FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE SET NULL;
    END IF;
END
$$;
