-- payment_timeout is the timeout in seconds for each individual off-chain
-- payment.
ALTER TABLE loopout_swaps ADD payment_timeout INTEGER NOT NULL DEFAULT 0;
