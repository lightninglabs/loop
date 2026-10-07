-- Drop the on-chain cost from static address loop-ins.
ALTER TABLE static_address_swaps DROP COLUMN onchain_cost;
