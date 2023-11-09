-- static_address stores the static loop-in addresses that clients
-- cooperatively created with the server.
CREATE TABLE IF NOT EXISTS static_addresses (
    -- id is the auto-incrementing primary key for a static address.
    id INTEGER PRIMARY KEY,

    -- client_pubkey is the client side public taproot key that is used to
    -- construct the 2-of-2 MuSig2 taproot output that represents the static
    -- address.
    client_pubkey BYTEA NOT NULL,

    -- server_pubkey is the server side public taproot key that is used to
    -- construct the 2-of-2 MuSig2 taproot output that represents the static
    -- address.
    server_pubkey BYTEA NOT NULL,

    -- expiry denotes the CSV delay at which funds at a specific static address
    -- can be swept back to the client.
    expiry INT NOT NULL,

    -- client_key_family is the key family of the client public key from the
    -- client's lnd wallet.
    client_key_family INT NOT NULL,

    -- client_key_index is the key index of the client public key from the
    -- client's lnd wallet.
    client_key_index INT NOT NULL,

    -- pkscript is the witness program that represents the static address. It is
    -- unique amongst all static addresses.
    pkscript BYTEA NOT NULL UNIQUE,

    -- protocol_version is the protocol version that the swap was created with.
    -- Note that this version is not upgraded if the client upgrades or
    -- downgrades their protocol version for static address outputs already in
    -- use.
    protocol_version INTEGER NOT NULL
);