-- reservations contains all the information about a reservation.
CREATE TABLE IF NOT EXISTS reservations (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- reservation_id is the unique identifier for the reservation.
    reservation_id BLOB NOT NULL UNIQUE,

    -- client_pubkey is the public key of the client.
    client_pubkey BLOB NOT NULL,
    
    -- server_pubkey is the public key of the server.
    server_pubkey BLOB NOT NULL,

    -- expiry is the absolute expiry height of the reservation.
    expiry INTEGER NOT NULL,

    -- value is the value of the reservation.
    value BIGINT NOT NULL,

    -- client_key_family is the key family of the client.
    client_key_family INTEGER NOT NULL,

    -- client_key_index is the key index of the client.
    client_key_index INTEGER NOT NULL,

    -- initiation_height is the height at which the reservation was initiated.
    initiation_height INTEGER NOT NULL,

    -- tx_hash is the hash of the transaction that created the reservation.
    tx_hash BLOB,

    -- out_index is the index of the output that created the reservation.
    out_index INTEGER,

    -- confirmation_height is the height at which the reservation was confirmed.
    confirmation_height INTEGER
);

CREATE INDEX IF NOT EXISTS reservations_reservation_id_idx ON reservations(reservation_id);

-- reservation_updates contains all the updates to a reservation.
CREATE TABLE IF NOT EXISTS reservation_updates (
    -- id is the auto incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- reservation_id is the unique identifier for the reservation.
    reservation_id BLOB NOT NULL REFERENCES reservations(reservation_id),

    -- update_state is the state of the reservation at the time of the update.
    update_state TEXT NOT NULL,
 
    -- update_timestamp is the timestamp of the update.
    update_timestamp TIMESTAMP NOT NULL
);

