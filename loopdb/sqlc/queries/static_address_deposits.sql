-- name: CreateDeposit :exec
INSERT INTO deposits (
    deposit_id,
    tx_hash,
    out_index,
    amount,
    confirmation_height,
    timeout_sweep_pk_script,
    expiry_sweep_txid,
    finalized_withdrawal_tx,
                      static_address_id

) VALUES (
             $1,
             $2,
             $3,
             $4,
             $5,
             $6,
             $7,
             $8,
             $9
         );

-- name: UpdateDeposit :exec
UPDATE deposits
SET
    tx_hash = $2,
    out_index = $3,
    confirmation_height = $4,
    expiry_sweep_txid = $5,
    finalized_withdrawal_tx = $6
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
    d.*,
    sa.client_pubkey     client_pubkey,
    sa.server_pubkey     server_pubkey,
    sa.expiry            expiry,
    sa.client_key_family client_key_family,
    sa.client_key_index  client_key_index,
    sa.pkscript          pkscript,
    sa.protocol_version  protocol_version,
    sa.initiation_height initiation_height
FROM deposits d
         LEFT JOIN static_addresses sa ON sa.id = d.static_address_id
WHERE
    deposit_id = $1;

-- name: DepositForOutpoint :one
SELECT
    d.*,
    sa.client_pubkey     client_pubkey,
    sa.server_pubkey     server_pubkey,
    sa.expiry            expiry,
    sa.client_key_family client_key_family,
    sa.client_key_index  client_key_index,
    sa.pkscript          pkscript,
    sa.protocol_version  protocol_version,
    sa.initiation_height initiation_height
FROM deposits d
         LEFT JOIN static_addresses sa ON sa.id = d.static_address_id
WHERE
    tx_hash = $1
AND
    out_index = $2;

-- name: AllDeposits :many
SELECT
    d.*,
    sa.client_pubkey     client_pubkey,
    sa.server_pubkey     server_pubkey,
    sa.expiry            expiry,
    sa.client_key_family client_key_family,
    sa.client_key_index  client_key_index,
    sa.pkscript          pkscript,
    sa.protocol_version  protocol_version,
    sa.initiation_height initiation_height
FROM deposits d
         LEFT JOIN static_addresses sa ON sa.id = d.static_address_id
ORDER BY d.id ASC;

-- name: GetLatestDepositUpdate :one
SELECT
    *
FROM
    deposit_updates
WHERE
    deposit_id = $1
ORDER BY
    update_timestamp DESC
LIMIT 1;

-- name: SetAllNullDepositsStaticAddressID :exec
UPDATE deposits
SET
    static_address_id = $1
WHERE
    static_address_id IS NULL;