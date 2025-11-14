-- Add a reference from deposits to static_addresses
ALTER TABLE deposits ADD COLUMN static_address_id INTEGER REFERENCES static_addresses(id);

CREATE INDEX IF NOT EXISTS deposits_static_address_id_index ON deposits(static_address_id);
