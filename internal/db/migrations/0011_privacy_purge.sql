-- 0011: privacy purge de auditoría al borrar un proyecto.
--
-- La auditoría es append-only, pero un borrado de proyecto no puede dejar
-- nombres de proyecto/destino, prefijos de token y `detail.to` colgando para
-- siempre. En la transacción de borrado se anonimizan las filas del proyecto
-- (project_id/token_prefix/subject/detail) y se conserva action/created_at como
-- evento agregado. El tombstone global `project.deleted` lo inserta la API.
--
-- El flag `ravenhub.privacy_purge` es transaction-local y solo permite UPDATE;
-- el flag de retención (`ravenhub.retention_sweep`) sigue permitiendo solo
-- DELETE. Cualquier otro UPDATE/DELETE sigue bloqueado.

CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    IF coalesce(current_setting('ravenhub.privacy_purge', true), '') = 'on' THEN
        IF TG_OP = 'UPDATE' THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'audit_log: privacy purge solo permite UPDATE';
    END IF;
    IF coalesce(current_setting('ravenhub.retention_sweep', true), '') = 'on' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'audit_log es append-only (el barrido de retención solo borra)';
    END IF;
    RAISE EXCEPTION 'audit_log es append-only';
END;
$$ LANGUAGE plpgsql;

-- Índice para que la limpieza/borrado no haga seq scan de toda la auditoría.
CREATE INDEX IF NOT EXISTS idx_audit_project_created
    ON audit_log(project_id, created_at DESC);

-- Limpieza única de las filas huérfanas de proyectos borrados con la versión
-- anterior. No toca filas de proyectos vivos.
SELECT set_config('ravenhub.privacy_purge', 'on', true);
UPDATE audit_log a
   SET project_id = NULL,
       token_prefix = '',
       subject = '',
       detail = '{}'::jsonb
 WHERE a.project_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.id = a.project_id);

-- Integridad: un InsertAudit concurrente ya no puede dejar un project_id
-- huérfano tras el borrado (la FK falla y el insert best-effort se descarta).
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
