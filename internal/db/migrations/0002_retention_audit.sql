-- Retención de auditoría: el trigger append-only permite
-- borrar SOLO dentro de transacciones marcadas como barrido de retención
-- (SET LOCAL ravenhub.retention_sweep='on' — ver store.DeleteOldAudit).
-- Cualquier UPDATE/DELETE fuera de ese barrido sigue bloqueado.
CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    IF coalesce(current_setting('ravenhub.retention_sweep', true), '') = 'on' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD; -- única excepción: el barrido de retención
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log es append-only';
END;
$$ LANGUAGE plpgsql;
