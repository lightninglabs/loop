-- Asset anchors use the normal deposit inventory and state history. Native
-- asset amounts remain in the versioned metadata; amount is always satoshis.
ALTER TABLE deposits ADD COLUMN asset_data BLOB;
ALTER TABLE deposits ADD COLUMN asset_request_id BLOB;
ALTER TABLE deposits ADD COLUMN asset_consumed BOOLEAN NOT NULL DEFAULT FALSE;

-- A request exists before funding/offer RPCs. The same row subsequently owns
-- the execution record, so release and recovery progress commit atomically.
CREATE TABLE static_address_asset_requests (
    request_id BLOB PRIMARY KEY,
    job_data BLOB NOT NULL,
    swap_hash BLOB UNIQUE,
    record_data BLOB,
    revision BIGINT NOT NULL DEFAULT 0
);
