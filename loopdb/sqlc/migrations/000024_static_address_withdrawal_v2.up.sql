-- Withdrawal-specific durable intent and state; legacy withdrawal tables remain
-- unchanged so historical evidence is preserved during application migration.
CREATE TABLE IF NOT EXISTS static_address_withdrawals_v2 (
    withdrawal_id TEXT PRIMARY KEY NOT NULL CHECK (length(withdrawal_id) = 64),
    version BIGINT NOT NULL CHECK (version > 0 AND version <= 4294967295),
    revision BIGINT NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (length(state) > 0),
    workflow_data BLOB NOT NULL CHECK (length(workflow_data) > 0),
    initiation_time TIMESTAMP NOT NULL,
    legacy_source TEXT UNIQUE
);

CREATE TABLE IF NOT EXISTS static_address_withdrawal_v2_deposits (
    withdrawal_id TEXT NOT NULL REFERENCES static_address_withdrawals_v2(withdrawal_id),
    deposit_id BLOB NOT NULL REFERENCES deposits(deposit_id),
    PRIMARY KEY (withdrawal_id, deposit_id)
);

-- Historical membership can overlap; only an active claim is exclusive.
CREATE TABLE IF NOT EXISTS static_address_withdrawal_v2_claims (
    deposit_id BLOB PRIMARY KEY NOT NULL REFERENCES deposits(deposit_id),
    withdrawal_id TEXT NOT NULL REFERENCES static_address_withdrawals_v2(withdrawal_id)
);

CREATE TABLE IF NOT EXISTS static_address_withdrawal_v2_updates (
    withdrawal_id TEXT NOT NULL REFERENCES static_address_withdrawals_v2(withdrawal_id),
    revision BIGINT NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (length(state) > 0),
    update_timestamp TIMESTAMP NOT NULL,
    PRIMARY KEY (withdrawal_id, revision)
);
