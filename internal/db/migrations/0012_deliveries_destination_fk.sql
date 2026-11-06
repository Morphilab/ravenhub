-- 0012: integridad de deliveries al borrar un destino.
--
-- deliveries.destination_id no tenía FK: borrar un destino dejaba IDs
-- colgantes y el histórico (dest_name/error) hasta que expirase el mensaje.
-- ON DELETE SET NULL conserva el histórico de entrega pero elimina la
-- referencia a una fila que ya no existe. Limpiamos los colgantes previos.

CREATE INDEX IF NOT EXISTS idx_deliveries_destination
    ON deliveries(destination_id);

UPDATE deliveries d
   SET destination_id = NULL
 WHERE d.destination_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM destinations x WHERE x.id = d.destination_id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'deliveries_destination_id_fkey'
    ) THEN
        ALTER TABLE deliveries
            ADD CONSTRAINT deliveries_destination_id_fkey
            FOREIGN KEY (destination_id) REFERENCES destinations(id) ON DELETE SET NULL;
    END IF;
END
$$;
