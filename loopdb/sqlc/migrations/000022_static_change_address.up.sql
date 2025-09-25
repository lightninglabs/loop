-- change_address stores the change output address for the HTLC transaction of a
-- static address loop-in swap.
ALTER TABLE static_address_swaps ADD change_address TEXT NOT NULL DEFAULT '';
