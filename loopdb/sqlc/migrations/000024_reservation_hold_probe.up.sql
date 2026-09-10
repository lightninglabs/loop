ALTER TABLE asset_reservations ADD COLUMN probe_node_key BLOB;
ALTER TABLE asset_reservations ADD COLUMN probe_request BLOB;
ALTER TABLE asset_reservations ADD COLUMN probe_result BLOB;
ALTER TABLE asset_reservations ADD COLUMN probe_deadline TIMESTAMP;
