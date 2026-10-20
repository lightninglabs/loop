ALTER TABLE loopout_swaps ADD COLUMN payment_address BYTEA
    CHECK (payment_address IS NULL OR length(payment_address) = 32);
