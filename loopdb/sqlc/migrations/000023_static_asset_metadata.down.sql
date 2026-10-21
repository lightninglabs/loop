ALTER TABLE static_address_swaps DROP COLUMN asset_data;
ALTER TABLE deposits DROP COLUMN asset_tx;
ALTER TABLE deposits DROP COLUMN asset_data;
ALTER TABLE swaps ALTER COLUMN preimage SET NOT NULL;
