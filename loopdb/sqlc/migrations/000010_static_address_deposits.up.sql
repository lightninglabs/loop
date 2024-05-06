-- deposits stores historic and unspent static address outputs.
CREATE TABLE IF NOT EXISTS deposits (
    -- id is the auto-incrementing primary key for a static address.
    id INTEGER PRIMARY KEY,

    -- deposit_id is the unique identifier for the deposit.
    deposit_id BLOB NOT NULL UNIQUE,

    -- tx_hash is the transaction hash of the deposit.
    tx_hash BYTEA NOT NULL,

    -- output_index is the index of the output in the transaction.
    out_index INT NOT NULL,

    -- amount is the amount of the deposit.
    amount BIGINT NOT NULL,

    -- confirmation_height is the absolute height at which the deposit was
    -- confirmed.
    confirmation_height BIGINT NOT NULL,

    -- timeout_sweep_pk_script is the public key script that will be used to
    -- sweep the deposit after has expired.
    timeout_sweep_pk_script BYTEA NOT NULL,

    -- expiry_sweep_txid is the transaction id of the expiry sweep.
    expiry_sweep_txid BLOB,

    -- withdrawal_sweep_address is the address that will be used to sweep the
    -- deposit cooperatively with the server before it has expired.
    withdrawal_sweep_address TEXT
);

-- deposit_updates contains all the updates to a deposit.
CREATE TABLE IF NOT EXISTS deposit_updates (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- deposit_id is the unique identifier for the deposit.
    deposit_id BLOB NOT NULL REFERENCES deposits(deposit_id),

    -- update_state is the state of the deposit at the time of the update.
    update_state TEXT NOT NULL,

    -- update_timestamp is the timestamp of the update.
    update_timestamp TIMESTAMP NOT NULL
);
