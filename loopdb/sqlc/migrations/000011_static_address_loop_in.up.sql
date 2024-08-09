-- static_address_swaps...
CREATE TABLE IF NOT EXISTS static_address_swaps (
    -- swap_hash is the primary identifier of the swap
    swap_hash BLOB PRIMARY KEY,

    -- swap_invoice is the invoice that needs to be paid by the server to
    -- complete the loop-in swap.
    swap_invoice TEXT NOT NULL,

    -- last_hop is an optional parameter that specifies the last hop to be
    -- used for a loop in swap.
    last_hop BLOB,

    -- quoted_swap_fee is the swap fee in sats that the server returned in
    -- the swap quote.
    quoted_swap_fee BIGINT NOT NULL,

    -- deposit_outpoints is a list of outpoints in the format txid:idx that are
    -- used for this swap.
    deposit_outpoints BLOB NOT NULL,

    -- htlc_tx contains the htlc transaction without signatures.
    htlc_tx BLOB,

    -- htlc_tx_fee_rate is the fee rate in sat/kw that is used for the htlc
    -- transaction.
    htlc_tx_fee_rate BIGINT NOT NULL,

    -- htlc_timeout_sweep_tx contains the finalized htlc timeout sweep
    -- transaction.
    htlc_timeout_sweep_tx BLOB,

    -- htlc_timeout_sweep_address contains the address the htlc timeout sweep
    -- transaction sends funds to.
    htlc_timeout_sweep_address TEXT
);


-- static_address_swap_updates contains all the updates to a loop-in swap.
CREATE TABLE IF NOT EXISTS static_address_swap_updates (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- deposit_id is the unique identifier for the deposit.
    swap_hash BLOB NOT NULL REFERENCES static_address_swaps(swap_hash),

    -- update_state is the state of the loop-in at the time of the update.
    update_state TEXT NOT NULL,

    -- update_timestamp is the timestamp of the update.
    update_timestamp TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS static_address_swap_hash_idx ON static_address_swap_updates(swap_hash);
