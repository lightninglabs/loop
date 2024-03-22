-- name: CreateDeposit :exec
INSERT INTO deposits (
    deposit_id,
    tx_hash,
    out_index,
    amount,
    confirmation_height,
    time_out_sweep_pk_script
) VALUES (
             $1,
             $2,
             $3,
             $4,
             $5,
             $6
         );

-- name: UpdateDeposit :exec
UPDATE deposits
SET
    tx_hash = $2,
    out_index = $3,
    confirmation_height = $4
WHERE
        deposits.deposit_id = $1;

-- name: InsertDepositUpdate :exec
INSERT INTO deposit_updates (
    deposit_id,
    update_state,
    update_timestamp
) VALUES (
             $1,
             $2,
             $3
         );

-- name: GetDeposit :one
SELECT
    *
FROM
    deposits
WHERE
        deposit_id = $1;

-- name: AllDeposits :many
SELECT
    *
FROM
    deposits
ORDER BY
    id ASC;

-- name: GetDepositUpdates :many
SELECT
    deposit_updates.*
FROM
    deposit_updates
WHERE
        deposit_id = $1
ORDER BY
    id ASC;