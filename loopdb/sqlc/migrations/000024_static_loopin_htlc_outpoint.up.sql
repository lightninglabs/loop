ALTER TABLE static_address_swaps
    ADD confirmed_htlc_tx_id TEXT;

ALTER TABLE static_address_swaps
    ADD confirmed_htlc_output_index INTEGER;

ALTER TABLE static_address_swaps
    ADD confirmed_htlc_output_value BIGINT;
