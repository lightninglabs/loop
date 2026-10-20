DROP TABLE static_address_asset_requests;
ALTER TABLE deposits DROP COLUMN asset_consumed;
ALTER TABLE deposits DROP COLUMN asset_request_id;
ALTER TABLE deposits DROP COLUMN asset_data;
