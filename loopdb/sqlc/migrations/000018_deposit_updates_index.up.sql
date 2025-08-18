CREATE INDEX IF NOT EXISTS deposit_updates_id_timestamp_index ON deposit_updates(deposit_id, update_timestamp DESC);
