-- onchain_cost is the client-side on-chain cost of a static address deposit
-- in satoshis: its share of the fee of the transaction that funded it.
ALTER TABLE deposits ADD COLUMN onchain_cost BIGINT NOT NULL DEFAULT 0;

-- onchain_cost_known tells whether onchain_cost is known: true if the client
-- wallet knows the funding fee, false if it doesn't, for example for a deposit
-- funded by an external wallet. NULL means the cost hasn't been determined yet.
ALTER TABLE deposits ADD COLUMN onchain_cost_known BOOLEAN;
