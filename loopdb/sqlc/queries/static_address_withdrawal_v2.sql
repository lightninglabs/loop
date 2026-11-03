-- name: CreateWithdrawalWorkflow :execrows
INSERT INTO static_address_withdrawals_v2 (
    withdrawal_id, version, revision, state, workflow_data, initiation_time,
    legacy_source
) VALUES (
    sqlc.arg(withdrawal_id), sqlc.arg(version), sqlc.arg(revision),
    sqlc.arg(state), sqlc.arg(workflow_data), sqlc.arg(initiation_time),
    sqlc.narg(legacy_source)
)
ON CONFLICT (withdrawal_id) DO NOTHING;

-- name: GetWithdrawalWorkflow :one
SELECT * FROM static_address_withdrawals_v2
WHERE withdrawal_id = $1;

-- name: GetWithdrawalWorkflowByLegacySource :one
SELECT * FROM static_address_withdrawals_v2
WHERE legacy_source = $1;

-- name: GetWithdrawalWorkflows :many
SELECT * FROM static_address_withdrawals_v2
ORDER BY initiation_time DESC, withdrawal_id ASC;

-- name: UpdateWithdrawalWorkflow :execrows
UPDATE static_address_withdrawals_v2
SET
    revision = sqlc.arg(revision),
    state = sqlc.arg(state),
    workflow_data = sqlc.arg(workflow_data)
WHERE
    withdrawal_id = sqlc.arg(withdrawal_id) AND
    revision = sqlc.arg(expected_revision) AND
    version = sqlc.arg(version);

-- name: CreateWithdrawalWorkflowDeposit :exec
INSERT INTO static_address_withdrawal_v2_deposits (withdrawal_id, deposit_id)
VALUES ($1, $2);

-- name: GetWithdrawalWorkflowDeposits :many
SELECT deposit_id FROM static_address_withdrawal_v2_deposits
WHERE withdrawal_id = $1
ORDER BY deposit_id ASC;

-- name: ClaimWithdrawalWorkflowDeposit :execrows
INSERT INTO static_address_withdrawal_v2_claims (deposit_id, withdrawal_id)
VALUES ($1, $2)
ON CONFLICT (deposit_id) DO NOTHING;

-- name: GetWithdrawalWorkflowClaim :one
SELECT withdrawal_id FROM static_address_withdrawal_v2_claims
WHERE deposit_id = $1;

-- name: ReleaseWithdrawalWorkflowClaims :exec
DELETE FROM static_address_withdrawal_v2_claims
WHERE withdrawal_id = $1;

-- name: InsertWithdrawalWorkflowUpdate :exec
INSERT INTO static_address_withdrawal_v2_updates (
    withdrawal_id, revision, state, update_timestamp
) VALUES ($1, $2, $3, $4);

-- name: GetWithdrawalWorkflowUpdates :many
SELECT * FROM static_address_withdrawal_v2_updates
WHERE withdrawal_id = $1
ORDER BY revision ASC;

-- name: GetWithdrawalMigrationDepositUpdate :one
SELECT * FROM deposit_updates
WHERE deposit_id = $1
ORDER BY update_timestamp DESC, id DESC
LIMIT 1;

-- name: UpdateWithdrawalWorkflowDepositTx :exec
UPDATE deposits SET finalized_withdrawal_tx = $2
WHERE deposit_id = $1;

-- name: CountWithdrawalDepositUpdatesAtTime :one
SELECT COUNT(*) FROM deposit_updates
WHERE deposit_id = $1 AND update_timestamp = $2;

-- name: GetActiveWithdrawalWorkflowMembership :many
SELECT members.withdrawal_id
FROM static_address_withdrawal_v2_deposits AS members
JOIN static_address_withdrawals_v2 AS workflows
    ON workflows.withdrawal_id = members.withdrawal_id
WHERE members.deposit_id = $1 AND workflows.state NOT IN ('Succeeded', 'Failed')
ORDER BY members.withdrawal_id ASC;
