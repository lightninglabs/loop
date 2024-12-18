-- static_address_swaps stores the static address loop-in specific data.
CREATE TABLE IF NOT EXISTS static_address_swaps (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- swap_hash of the swap is unique and is used to identify the swap.
    swap_hash BLOB NOT NULL UNIQUE,

    -- swap_invoice is the invoice that needs to be paid by the server to
    -- complete the loop-in swap.
    swap_invoice TEXT NOT NULL,

    -- last_hop is an optional parameter that specifies the last hop to be
    -- used for a loop in swap.
    last_hop BLOB,

    -- payment_timeout_seconds is the time in seconds that the server has to
    -- pay the invoice.
    payment_timeout_seconds INTEGER NOT NULL,

    -- quoted_swap_fee_satoshis is the swap fee in sats that the server returned
    -- in the swap quote.
    quoted_swap_fee_satoshis BIGINT NOT NULL,

    -- deposit_outpoints is a concatenated list of outpoints that are used for
    -- this swap. The list has the format txid1:idx;txid2:idx;...
    deposit_outpoints TEXT NOT NULL,

    -- htlc_tx_fee_rate_sat_kw is the fee rate in sat/kw that is used for the
    -- htlc transaction.
    htlc_tx_fee_rate_sat_kw BIGINT NOT NULL,

    -- htlc_timeout_sweep_tx_hash contains the htlc timeout sweep tx id.
    htlc_timeout_sweep_tx_id TEXT,

    -- htlc_timeout_sweep_address contains the address the htlc timeout sweep
    -- transaction sends funds to.
    htlc_timeout_sweep_address TEXT NOT NULL
);

-- static_address_swap_updates contains all the updates to a loop-in swap.
CREATE TABLE IF NOT EXISTS static_address_swap_updates (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- deposit_id is the unique identifier for the deposit.
    swap_hash BLOB NOT NULL REFERENCES static_address_swaps(swap_hash),

    -- update_state is the state of the loop-in at the time of the update.
    -- Example states are InitHtlc, SignHtlcTx and others defined in
    -- staticaddr/loopin/fsm.go.
    update_state TEXT NOT NULL,

    -- update_timestamp is the timestamp of the update.
    update_timestamp TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS static_address_swap_hash_idx ON static_address_swap_updates(swap_hash);
CREATE INDEX IF NOT EXISTS static_address_update_state_idx ON static_address_swap_updates(update_state);
