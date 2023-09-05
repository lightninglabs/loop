-- sweep_batches stores the on-going swaps that are batched together.
CREATE TABLE sweep_batches (
        -- id is the autoincrementing primary key of the batch.
        id INTEGER PRIMARY KEY,

        -- confirmed indicates whether this batch is confirmed.
        confirmed BOOLEAN NOT NULL DEFAULT FALSE,

        -- batch_tx_id is the transaction id of the batch transaction.
        batch_tx_id TEXT,

        -- batch_pk_script is the pkscript of the batch transaction's output.
        batch_pk_script BLOB,

        -- last_rbf_height was the last height at which we attempted to publish
        -- an rbf replacement transaction. 
        last_rbf_height INTEGER,

        -- last_rbf_sat_per_kw was the last sat per kw fee rate we used for the
        -- last published transaction.
        last_rbf_sat_per_kw INTEGER,

        -- max_timeout_distance is the maximum distance the timeouts of the
        -- sweeps can have in the batch.
        max_timeout_distance INTEGER NOT NULL
);

-- sweeps stores the individual sweeps that are part of a batch.
CREATE TABLE sweeps (
        -- id is the autoincrementing primary key.
        id INTEGER PRIMARY KEY,

        -- swap_hash is the hash of the swap that is being swept.
        swap_hash BLOB NOT NULL UNIQUE,

        -- batch_id is the id of the batch this swap is part of.
        batch_id INTEGER NOT NULL,

        -- outpoint_txid is the transaction id of the output being swept.
        outpoint_txid BLOB NOT NULL,

        -- outpoint_index is the index of the output being swept.
        outpoint_index INTEGER NOT NULL,

        -- amt is the amount of the output being swept.
        amt BIGINT NOT NULL,

        -- completed indicates whether the sweep has been completed.
        completed BOOLEAN NOT NULL DEFAULT FALSE,

        -- Foreign key constraint to ensure that we reference an existing batch
        -- id.
        FOREIGN KEY (batch_id) REFERENCES sweep_batches(id),

        -- Foreign key constraint to ensure that swap_hash references an
        -- existing swap.
        FOREIGN KEY (swap_hash) REFERENCES swaps(swap_hash)
);
