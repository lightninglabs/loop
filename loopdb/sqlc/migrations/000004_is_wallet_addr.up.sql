-- is_external_addr indicates whether the destination address of the swap is not
-- a wallet address. The default value used is TRUE in order to maintain the old
-- behavior of swaps which doesn't override the destination address.
ALTER TABLE loopout_swaps ADD single_sweep BOOLEAN NOT NULL DEFAULT TRUE;