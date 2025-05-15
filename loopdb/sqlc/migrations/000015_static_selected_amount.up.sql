-- selected_amount is a fractional amount amongst selected deposits of a static
-- address loop-in swap. A selected amount of 0 indicates that the total amount
-- of deposits is selected for the swap.
ALTER TABLE static_address_swaps ADD selected_amount BIGINT NOT NULL DEFAULT 0;
