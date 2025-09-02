-- Drop index and column linking deposits to static_addresses
DROP INDEX IF EXISTS deposits_static_address_id_index;
ALTER TABLE deposits DROP COLUMN static_address_id;