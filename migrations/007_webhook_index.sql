-- webhook_deliveries indexes every verified GitHub webhook delivery, not only
-- the ones that changed a run. The raw payload goes to the telemetry store,
-- never here. outcome is handled, ignored or failed; error says why it failed.
-- Rows from before this migration keep NULLs: each of them was handled.
ALTER TABLE webhook_deliveries
 ADD COLUMN event text,
 ADD COLUMN action text,
 ADD COLUMN repository text,
 ADD COLUMN installation_id bigint,
 ADD COLUMN outcome text CHECK (outcome IN ('handled', 'ignored', 'failed')),
 ADD COLUMN error text;
