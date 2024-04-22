-- name: GetUnconfirmedBatches :many
SELECT
        *
FROM
        sweep_batches
WHERE
        confirmed = FALSE;

-- name: InsertBatch :one
INSERT INTO sweep_batches (
        confirmed,
        batch_tx_id,
        batch_pk_script,
        last_rbf_height,
        last_rbf_sat_per_kw,
        max_timeout_distance
) VALUES (
        $1,
        $2,
        $3,
        $4,
        $5,
        $6
) RETURNING id;

-- name: UpdateBatch :exec
UPDATE sweep_batches SET
        confirmed = $2,
        batch_tx_id = $3,
        batch_pk_script = $4,
        last_rbf_height = $5,
        last_rbf_sat_per_kw = $6
WHERE id = $1;

-- name: ConfirmBatch :exec
UPDATE
        sweep_batches
SET
        confirmed = TRUE
WHERE
        id = $1;

-- name: UpsertSweep :exec
INSERT INTO sweeps (
        swap_hash,
        batch_id,
        outpoint_txid,
        outpoint_index,
        amt,
        completed
) VALUES (
        $1,
        $2,
        $3,
        $4,
        $5,
        $6
) ON CONFLICT (swap_hash) DO UPDATE SET
        batch_id = $2,
        outpoint_txid = $3,
        outpoint_index = $4,
        amt = $5,
        completed = $6;

-- name: GetParentBatch :one
SELECT
        sweep_batches.*
FROM
        sweep_batches
JOIN
        sweeps ON sweep_batches.id = sweeps.batch_id
WHERE
        sweeps.swap_hash = $1
AND
        sweeps.completed = TRUE
AND   
        sweep_batches.confirmed = TRUE;

-- name: GetBatchSweptAmount :one
SELECT
        SUM(amt) AS total
FROM
        sweeps
WHERE
        batch_id = $1
AND
        completed = TRUE;

-- name: GetBatchSweeps :many
SELECT
        sweeps.*,
        swaps.*,
        loopout_swaps.*,
        htlc_keys.*
FROM
        sweeps
JOIN
        swaps ON sweeps.swap_hash = swaps.swap_hash
JOIN
        loopout_swaps ON sweeps.swap_hash = loopout_swaps.swap_hash
JOIN
        htlc_keys ON sweeps.swap_hash = htlc_keys.swap_hash
WHERE
        sweeps.batch_id = $1
ORDER BY
        sweeps.id ASC;

-- name: GetSweepStatus :one
SELECT
    COALESCE(s.completed, f.false_value) AS completed
FROM
    (SELECT false AS false_value) AS f
LEFT JOIN
    sweeps s ON s.swap_hash = $1;
