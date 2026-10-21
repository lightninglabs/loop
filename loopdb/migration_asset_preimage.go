package loopdb

// sqliteNullablePreimage rebuilds swaps because SQLite cannot drop a NOT NULL
// constraint. All identifiers and existing values are copied unchanged. The
// explicit foreign-key check verifies dependent rows before deferred checks
// are reset; SQLite otherwise remembers the temporary removal of swaps.
const sqliteNullablePreimage = `
PRAGMA defer_foreign_keys = ON;
CREATE TABLE swaps_nullable_preimage (
    id INTEGER PRIMARY KEY,
    swap_hash BLOB NOT NULL UNIQUE,
    preimage BLOB UNIQUE,
    initiation_time TIMESTAMP NOT NULL,
    amount_requested BIGINT NOT NULL,
    cltv_expiry INTEGER NOT NULL,
    max_miner_fee BIGINT NOT NULL,
    max_swap_fee BIGINT NOT NULL,
    initiation_height INTEGER NOT NULL,
    protocol_version INTEGER NOT NULL,
    label TEXT NOT NULL
);
INSERT INTO swaps_nullable_preimage SELECT * FROM swaps;
DROP TABLE swaps;
ALTER TABLE swaps_nullable_preimage RENAME TO swaps;
CREATE TEMP TABLE asset_preimage_fk_check (
    violations INTEGER CHECK (violations = 0)
);
INSERT INTO asset_preimage_fk_check
SELECT COUNT(*) FROM pragma_foreign_key_check;
DROP TABLE asset_preimage_fk_check;
PRAGMA defer_foreign_keys = OFF;
`
