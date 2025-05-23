-- name: CreateWithdrawal :exec
INSERT INTO withdrawals (
    withdrawal_tx_id,
    deposit_outpoints,
    total_deposit_amount,
    withdrawn_amount,
    change_amount,
    confirmation_height
) VALUES (
             $1,
             $2,
             $3,
             $4,
             $5,
             $6
         );

-- name: AllWithdrawals :many
SELECT
    *
FROM
    withdrawals
ORDER BY
    id ASC;