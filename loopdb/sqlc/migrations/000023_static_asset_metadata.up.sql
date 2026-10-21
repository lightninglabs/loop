-- Assets extend the ordinary deposit and static Loop In records. Native
-- quantities remain in asset_data; Bitcoin amount columns remain satoshis.
ALTER TABLE deposits ADD COLUMN asset_data BLOB;
ALTER TABLE deposits ADD COLUMN asset_tx BLOB;
ALTER TABLE static_address_swaps ADD COLUMN asset_data BLOB;

-- The client learns the server-controlled preimage only during execution.
ALTER TABLE swaps ALTER COLUMN preimage DROP NOT NULL;
