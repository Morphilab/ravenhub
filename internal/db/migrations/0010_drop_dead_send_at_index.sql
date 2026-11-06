-- 0010: idx_messages_send_at quedó MUERTO: el scheduler reclama por
-- next_attempt_at (send_at solo se proyecta como columna) y ninguna query
-- filtra por él — el índice parcial solo costaba escrituras en cada reenvío
-- programado.
DROP INDEX IF EXISTS idx_messages_send_at;
