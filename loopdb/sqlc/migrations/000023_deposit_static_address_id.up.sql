-- Existing deposits must belong to one unambiguous legacy address. A guard
-- table works on both backends without PostgreSQL's database TEMPORARY privilege.
CREATE TABLE migration_23_address_guard (
    address_count BIGINT NOT NULL
        CONSTRAINT migration_23_requires_one_legacy_address CHECK (address_count = 1)
);

INSERT INTO migration_23_address_guard (address_count)
SELECT (SELECT COUNT(*) FROM static_addresses)
WHERE EXISTS (SELECT 1 FROM deposits);

DROP TABLE migration_23_address_guard;

ALTER TABLE deposits ADD static_address_id INT REFERENCES static_addresses(id);

UPDATE deposits
SET static_address_id = (
    SELECT id FROM static_addresses ORDER BY id ASC LIMIT 1
)
WHERE static_address_id IS NULL
  AND EXISTS (SELECT 1 FROM static_addresses);
