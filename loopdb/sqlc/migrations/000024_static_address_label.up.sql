-- label records local operator metadata for a static address. It is not part of
-- the address script or server protocol state; an empty value means the address
-- is unlabeled.
ALTER TABLE static_addresses ADD COLUMN label TEXT NOT NULL DEFAULT '';
