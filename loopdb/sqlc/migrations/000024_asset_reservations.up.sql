-- Store agreed terms separately from the append-only state history.
CREATE TABLE asset_reservations (
    id INTEGER PRIMARY KEY,
    reservation_id BYTEA NOT NULL UNIQUE CHECK (length(reservation_id) = 32),
    asset_id BYTEA NOT NULL CHECK (length(asset_id) = 32),
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- Preserve the accepted quote; pricing policy belongs to the server.
    fee BIGINT NOT NULL CHECK (fee >= 0),
    csv_delay INTEGER NOT NULL CHECK (csv_delay BETWEEN 0 AND 65535),
    required_confirmations INTEGER NOT NULL CHECK (required_confirmations >= 0),
    execution_delta INTEGER NOT NULL CHECK (execution_delta >= 0),
    min_usable_blocks INTEGER NOT NULL CHECK (min_usable_blocks >= 0),
    client_pubkey BYTEA NOT NULL CHECK (length(client_pubkey) = 33),
    client_key_family INTEGER NOT NULL CHECK (client_key_family >= 0),
    client_key_index BIGINT NOT NULL
        CHECK (client_key_index BETWEEN 0 AND 4294967295),
    created_at TIMESTAMP NOT NULL,
    CHECK (amount <= 9223372036854775807 - fee),
    -- Before the quote, only the requested asset and amount are known.
    CHECK ((fee = 0 AND csv_delay = 0 AND required_confirmations = 0
        AND execution_delta = 0 AND min_usable_blocks = 0)
        OR (fee > 0 AND csv_delay > 0 AND required_confirmations > 0
        AND execution_delta > 0 AND min_usable_blocks > 0)),
    CHECK (CAST(required_confirmations AS BIGINT) + execution_delta
        + min_usable_blocks <= csv_delay)
);

CREATE TABLE asset_reservation_updates (
    id INTEGER PRIMARY KEY,
    reservation_id BYTEA NOT NULL REFERENCES asset_reservations(reservation_id),
    update_state TEXT NOT NULL CHECK (length(update_state) > 0),
    update_timestamp TIMESTAMP NOT NULL
);

CREATE INDEX asset_reservation_updates_id_idx
    ON asset_reservation_updates(reservation_id, id);
