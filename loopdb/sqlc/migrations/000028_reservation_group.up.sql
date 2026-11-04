ALTER TABLE asset_reservations ADD COLUMN group_key BYTEA
    CHECK (group_key IS NULL OR length(group_key) = 33);
