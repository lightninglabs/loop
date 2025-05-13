-- name: CreateWithdrawal :exec
INSERT INTO withdrawals (
    withdrawal_id,
    total_deposit_amount,
    initiation_time
) VALUES (
     $1, $2, $3
 );

-- name: CreateWithdrawalDeposit :exec
INSERT INTO withdrawal_deposits (
    withdrawal_id,
    deposit_id
) VALUES (
     $1, $2
);

-- name: GetWithdrawalIDByDepositID :one
SELECT withdrawal_id
FROM withdrawal_deposits
WHERE deposit_id = $1;

-- name: UpdateWithdrawal :exec
UPDATE withdrawals
SET
    withdrawal_tx_id = $2,
    withdrawn_amount = $3,
    change_amount = $4,
    confirmation_height = $5
WHERE
    withdrawal_id = $1;

-- name: GetWithdrawalDeposits :many
SELECT
    deposit_id
FROM
    withdrawal_deposits
WHERE
    withdrawal_id = $1;

-- name: GetAllWithdrawals :many
SELECT
    *
FROM
    withdrawals
ORDER BY
    initiation_time DESC;