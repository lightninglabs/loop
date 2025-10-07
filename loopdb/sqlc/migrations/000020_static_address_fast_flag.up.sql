-- Add 'fast' flag to static_address_swaps, default false
ALTER TABLE static_address_swaps ADD COLUMN fast BOOLEAN NOT NULL DEFAULT false;
