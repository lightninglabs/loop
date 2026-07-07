ALTER TABLE static_address_swaps
    DROP COLUMN confirmed_htlc_output_value;

ALTER TABLE static_address_swaps
    DROP COLUMN confirmed_htlc_output_index;

ALTER TABLE static_address_swaps
    DROP COLUMN confirmed_htlc_tx_id;
