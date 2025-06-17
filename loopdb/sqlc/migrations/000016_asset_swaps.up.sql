CREATE TABLE IF NOT EXISTS asset_swaps (
	--- id is the autoincrementing primary key.
	id INTEGER PRIMARY KEY,

	-- swap_hash is the randomly generated hash of the swap, which is used
	-- as the swap identifier for the clients.
	swap_hash BLOB NOT NULL UNIQUE,

        -- swap_preimage is the preimage of the swap.
        swap_preimage BLOB,

        -- asset_id is the identifier of the asset being swapped.
        asset_id BLOB NOT NULL,

        -- amt is the requested amount to be swapped.
        amt BIGINT NOT NULL,

        -- sender_pubkey is the pubkey of the sender.
        sender_pubkey BLOB NOT NULL,

        -- receiver_pubkey is the pubkey of the receiver.
        receiver_pubkey BLOB NOT NULL,

        -- csv_expiry is the expiry of the swap.
        csv_expiry INTEGER NOT NULL,

        -- server_key_family is the family of key being identified.
	server_key_family BIGINT NOT NULL,

	-- server_key_index is the precise index of the key being identified.
        server_key_index BIGINT NOT NULL,

        -- initiation_height is the height at which the swap was initiated.
        initiation_height INTEGER NOT NULL,

        -- created_time is the time at which the swap was created.
        created_time TIMESTAMP NOT NULL,

        -- htlc_confirmation_height is the height at which the swap was confirmed.
        htlc_confirmation_height INTEGER NOT NULL DEFAULT(0),

        -- htlc_txid is the txid of the confirmation transaction.
        htlc_txid BLOB,

        -- htlc_vout is the vout of the confirmation transaction.
        htlc_vout INTEGER NOT NULL DEFAULT (0),

        -- sweep_txid is the txid of the sweep transaction.
        sweep_txid BLOB,

        -- sweep_confirmation_height is the height at which the swap was swept.
        sweep_confirmation_height INTEGER NOT NULL DEFAULT(0),

        sweep_pkscript BLOB
);


CREATE TABLE IF NOT EXISTS asset_swaps_updates (
        -- id is auto incremented for each update.
        id INTEGER PRIMARY KEY,

        -- swap_hash is the hash of the swap that this update is for.
        swap_hash BLOB NOT NULL REFERENCES asset_swaps(swap_hash),

        -- update_state is the state of the swap at the time of the update.
        update_state TEXT NOT NULL,

        -- update_timestamp is the time at which the update was created.
        update_timestamp TIMESTAMP NOT NULL
);


CREATE INDEX IF NOT EXISTS asset_swaps_updates_swap_hash_idx ON asset_swaps_updates(swap_hash);


CREATE TABLE IF NOT EXISTS asset_out_swaps (
        -- swap_hash is the identifier of the swap.
        swap_hash BLOB PRIMARY KEY REFERENCES asset_swaps(swap_hash),

       -- raw_proof_file is the file containing the raw proof.
        raw_proof_file BLOB
);

CREATE INDEX IF NOT EXISTS asset_out_swaps_swap_hash_idx ON asset_out_swaps(swap_hash);

