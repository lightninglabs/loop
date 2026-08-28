ALTER TABLE static_address_swaps
    ADD change_static_address_id INT REFERENCES static_addresses(id);

-- Existing fractional swaps sent change back to the legacy static address.
-- Backfill that relation so in-flight swaps remain recoverable after the
-- client starts requiring explicit per-swap change metadata.
UPDATE static_address_swaps
SET change_static_address_id = (
    SELECT id FROM static_addresses ORDER BY id ASC LIMIT 1
)
WHERE selected_amount > 0
  AND change_static_address_id IS NULL
  AND EXISTS (SELECT 1 FROM static_addresses);
