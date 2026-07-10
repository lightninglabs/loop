ALTER TABLE deposits ADD static_address_id INT REFERENCES static_addresses(id);

UPDATE deposits
SET static_address_id = (
    SELECT id FROM static_addresses ORDER BY id ASC LIMIT 1
)
WHERE static_address_id IS NULL
  AND EXISTS (SELECT 1 FROM static_addresses);
