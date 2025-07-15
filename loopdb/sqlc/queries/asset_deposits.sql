-- name: AddAssetDeposit :exec
INSERT INTO asset_deposits (
    deposit_id,
    protocol_version,
    created_at,
    asset_id,
    amount,
    client_script_pubkey,
    server_script_pubkey,
    client_internal_pubkey,
    server_internal_pubkey,
    server_internal_key,
    client_key_family,
    client_key_index,
    expiry,
    addr
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);

-- name: UpdateDepositState :exec
INSERT INTO asset_deposit_updates (
    deposit_id,
    update_state,
    update_timestamp
) VALUES ($1, $2, $3);

-- name: MarkDepositConfirmed :exec
UPDATE asset_deposits 
SET confirmation_height = $2, outpoint = $3, pk_script = $4
WHERE deposit_id = $1;

-- name: GetAssetDeposits :many
SELECT d.*, u.update_state, u.update_timestamp
FROM asset_deposits d
JOIN asset_deposit_updates u ON u.id = (
    SELECT id
    FROM asset_deposit_updates
    WHERE deposit_id = d.deposit_id
    ORDER BY update_timestamp DESC
    LIMIT 1
)
ORDER BY d.created_at ASC;
