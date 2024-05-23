-- payment_timeout is the timeout in seconds for each individual off-chain
-- payment.
ALTER TABLE loopout_swaps DROP COLUMN payment_timeout;
