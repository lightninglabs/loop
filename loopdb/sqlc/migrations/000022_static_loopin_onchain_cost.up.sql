-- onchain_cost is the client-side on-chain cost of a static address loop-in in
-- satoshis, from the fees of its deposit funding transactions. NULL means the
-- cost is unknown.
ALTER TABLE static_address_swaps ADD COLUMN onchain_cost BIGINT;
