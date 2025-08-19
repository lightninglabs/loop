CREATE TABLE IF NOT EXISTS asset_deposits (
    deposit_id TEXT PRIMARY KEY,

    -- protocol_version is the protocol version that the deposit was
    -- created with.
    protocol_version INTEGER NOT NULL,

    -- created_at is the time at which the deposit was created.
    created_at TIMESTAMP NOT NULL,

    -- asset_id is the asset that is being deposited.
    asset_id BLOB NOT NULL,

    -- amount is the amount of the deposit in asset units.
    amount BIGINT NOT NULL,

    -- client_script_pubkey is the key used for the deposit script path as well
    -- as the ephemeral key used for deriving the client's internal key.
    client_script_pubkey BLOB NOT NULL,

    -- server_script_pubkey is the server's key that is used to construct the
    -- deposit spending HTLC.
    server_script_pubkey BLOB NOT NULL,

    -- client_internal_pubkey is the key derived from the shared secret
    -- which is derived from the client's script key.
    client_internal_pubkey BLOB NOT NULL,

    -- server_internal_pubkey is the server side public key that is used to
    -- construct the 2-of-2 MuSig2 anchor output that holds the deposited
    -- funds.
    server_internal_pubkey BLOB NOT NULL,

    -- server_internal_key is the revealed private key corresponding to the
    -- server's internal public key. It is only revealed when the deposit is
    -- cooperatively withdrawn and therefore may be NULL. Note that the value
    -- may be encrypted.
    server_internal_key BYTEA,

    -- expiry denotes the CSV delay at which funds at a specific static address
    -- can be swept back to the client.
    expiry INT NOT NULL,

    -- client_key_family is the key family of the client's script public key
    -- from the client's lnd wallet.
    client_key_family INT NOT NULL,

    -- client_key_index is the key index of the client's script public key from
    -- the client's lnd wallet.
    client_key_index INT NOT NULL,

    -- addr is the TAP deposit address that the client should send the funds to.
    addr TEXT NOT NULL UNIQUE,

    -- confirmation_height is the block height at which the deposit was
    -- confirmed on-chain.
    confirmation_height INT,

    -- outpoint is the outpoint of the confirmed deposit.
    outpoint TEXT,

    -- pk_script is the pkscript of the deposit anchor output.
    pk_script BLOB,

    -- sweep_script_pubkey is the script key that will be used for the asset 
    -- after the deposit is swept.
    sweep_script_pubkey BLOB,

    -- sweep_internal_pubkey is the internal public key that will be used
    -- for the asset output after the deposit is swept.
    sweep_internal_pubkey BLOB
);

-- asset_deposit_updates contains all the updates to an asset deposit.
CREATE TABLE IF NOT EXISTS asset_deposit_updates (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- deposit_id is the unique identifier for the deposit.
    deposit_id TEXT NOT NULL REFERENCES asset_deposits(deposit_id),

    -- update_state is the state of the deposit at the time of the update.
    update_state INT NOT NULL,

    -- update_timestamp is the timestamp of the update.
    update_timestamp TIMESTAMP NOT NULL
);

-- asset_deposit_leased_utxos contains all the UTXOs that were leased to a
-- particular deposit. These leased UTXOs are used to fund the deposit timeout
-- sweep transaction.
CREATE TABLE IF NOT EXISTS asset_deposit_leased_utxos (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- deposit_id is the unique identifier for the deposit.
    deposit_id TEXT NOT NULL REFERENCES asset_deposits(deposit_id),

    -- outpoint is the outpoint of the UTXO that was leased.
    outpoint TEXT NOT NULL
);
