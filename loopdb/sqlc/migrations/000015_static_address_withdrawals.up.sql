-- withdrawals stores finalized static address withdrawals.
CREATE TABLE IF NOT EXISTS withdrawals (
    -- id is the auto-incrementing primary key for a withdrawal.
    id INTEGER PRIMARY KEY,

    -- withdrawal_id is the unique identifier for the withdrawal.
    withdrawal_id BLOB NOT NULL UNIQUE,

    -- withdrawal_tx_id is the transaction tx id of the withdrawal.
    withdrawal_tx_id TEXT UNIQUE,

    -- total_deposit_amount is the total amount of the deposits in satoshis.
    total_deposit_amount BIGINT NOT NULL,

    -- withdrawn_amount is the total amount of the withdrawal. It amounts
    -- to the total amount of the deposits minus the fees and optional change.
    withdrawn_amount BIGINT,

    -- change_amount is the optional change that the user selected.
    change_amount BIGINT,

    -- initiation_time is the creation of the withdrawal.
    initiation_time TIMESTAMP NOT NULL,

    -- confirmation_height is the block height at which the withdrawal was first
    -- confirmed.
    confirmation_height BIGINT
);

CREATE TABLE IF NOT EXISTS withdrawal_deposits (
    -- id is the auto-incrementing primary key.
    id INTEGER PRIMARY KEY,

    -- withdrawal_id references the withdrawals table.
    withdrawal_id BLOB NOT NULL REFERENCES withdrawals(withdrawal_id),

    -- deposit_id references the deposits table.
    deposit_id BLOB NOT NULL REFERENCES deposits(deposit_id),

    -- Ensure that each deposit is used only once per withdrawal.
    UNIQUE(deposit_id, withdrawal_id)
);
