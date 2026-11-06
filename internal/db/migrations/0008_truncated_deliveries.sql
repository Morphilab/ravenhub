-- Truncado con rastro: un mensaje cortado por el límite del proveedor
-- (Telegram 4096, Discord 2000, Slack 4000, log 8000) deja de ser invisible.
-- deliveries.truncated + contador ravenhub_truncated_total{channel}.
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS truncated BOOLEAN NOT NULL DEFAULT FALSE;
