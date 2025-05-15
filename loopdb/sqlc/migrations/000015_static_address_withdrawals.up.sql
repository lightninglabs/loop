-- withdrawals stores finalized static address withdrawals.
CREATE TABLE IF NOT EXISTS withdrawals (
    -- id is the auto-incrementing primary key for a withdrawal.
    id INTEGER PRIMARY KEY,

    -- withdrawal_tx_id is the transaction tx id of the withdrawal.
    withdrawal_tx_id TEXT NOT NULL UNIQUE,

    -- deposit_outpoints is a concatenated list of outpoints that are used for
    -- this withdrawal. The list has the format txid1:idx;txid2:idx;...
    deposit_outpoints TEXT NOT NULL,

    -- total_deposit_amount is the total amount of the deposits in satoshis.
    total_deposit_amount BIGINT NOT NULL,

    -- withdrawn_amount is the total amount of the withdrawal. It amounts
    -- to the total amount of the deposits minus the fees and optional change.
    withdrawn_amount BIGINT NOT NULL,

    -- change_amount is the optional change that the user selected.
    change_amount BIGINT NOT NULL,

    -- confirmation_height is the block height at which the withdrawal was
    -- first confirmed.
    confirmation_height BIGINT NOT NULL
);