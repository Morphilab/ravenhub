-- 0007: el digest persiste las Idempotency-Key de los mensajes que pliega.
-- Antes, un reintento con la misma clave no encontraba su dedup (no existía
-- fila del mensaje plegado) y se contaba OTRA VEZ en aggregated_count: doble
-- conteo y respuesta aggregated en vez de duplicate.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS folded_keys TEXT[] NOT NULL DEFAULT '{}';
