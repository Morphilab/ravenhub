-- 0006: el escape hatch del barrido de retención solo autoriza DELETE.
-- Con ravenhub.retention_sweep='on', un UPDATE de audit_log falla; el
-- barrido solo puede borrar.
CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    IF coalesce(current_setting('ravenhub.retention_sweep', true), '') = 'on' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD; -- única excepción: el barrido de retención
        END IF;
        RAISE EXCEPTION 'audit_log es append-only (el barrido de retención solo borra)';
    END IF;
    RAISE EXCEPTION 'audit_log es append-only';
END;
$$ LANGUAGE plpgsql;
