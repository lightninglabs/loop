CREATE TABLE IF NOT EXISTS instantout_swaps (
	-- swap_hash points to the parent swap hash.
	swap_hash BLOB PRIMARY KEY,

        -- preimage is the preimage of the swap.
        preimage BLOB NOT NULL,

        -- sweep_address is the address that the server should sweep the funds to.
        sweep_address TEXT NOT NULL,

        -- outgoing_chan_set is the set of short ids of channels that may be used.
	-- If empty, any channel may be used.
	outgoing_chan_set TEXT NOT NULL,

        -- htlc_fee_rate is the fee rate in sat/kw that is used for the htlc transaction.
        htlc_fee_rate BIGINT NOT NULL,

        -- reservation_ids is a list of ids of the reservations that are used for this swap.
        reservation_ids BLOB NOT NULL,

         -- swap_invoice is the invoice that is to be paid by the client to
	-- initiate the loop out swap.
	swap_invoice TEXT NOT NULL,

        -- finalized_htlc_tx contains the fully signed htlc transaction.
        finalized_htlc_tx BLOB,

        -- sweep_tx_hash is the hash of the transaction that sweeps the htlc.
        sweep_tx_hash BLOB,

        -- finalized_sweepless_sweep_tx contains the fully signed sweepless sweep transaction.
        finalized_sweepless_sweep_tx BLOB,

        -- sweep_confirmation_height is the block height at which the sweep transaction is confirmed.
        sweep_confirmation_height INTEGER
);

CREATE TABLE IF NOT EXISTS instantout_updates (
        -- id is auto incremented for each update.
        id INTEGER PRIMARY KEY,

        -- swap_hash is the hash of the swap that this update is for.
        swap_hash BLOB NOT NULL REFERENCES instantout_swaps(swap_hash),

        -- update_state is the state of the swap at the time of the update.
        update_state TEXT NOT NULL,

        -- update_timestamp is the time at which the update was created.
        update_timestamp TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS instantout_updates_swap_hash_idx ON instantout_updates(swap_hash);
