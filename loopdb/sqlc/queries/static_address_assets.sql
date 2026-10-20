-- name: GetAssetRequest :one
SELECT * FROM static_address_asset_requests WHERE request_id = $1;

-- name: GetAssetRecord :one
SELECT * FROM static_address_asset_requests WHERE swap_hash = $1;

-- name: ListAssetRequests :many
SELECT * FROM static_address_asset_requests ORDER BY request_id;

-- name: InsertAssetRequest :exec
INSERT INTO static_address_asset_requests(request_id, job_data)
VALUES ($1, $2);

-- name: UpdateAssetJob :exec
UPDATE static_address_asset_requests SET job_data = $2 WHERE request_id = $1;

-- name: InsertAssetRecord :execrows
UPDATE static_address_asset_requests
SET swap_hash = $2, record_data = $3, revision = $4
WHERE request_id = $1 AND record_data IS NULL;

-- name: UpdateAssetRecord :execrows
UPDATE static_address_asset_requests SET record_data = $2, revision = $3
WHERE swap_hash = $1 AND revision = $4;

-- name: ListAssetDeposits :many
SELECT * FROM deposits WHERE asset_data IS NOT NULL ORDER BY id;

-- name: GetAssetDeposit :one
SELECT * FROM deposits WHERE tx_hash = $1 AND out_index = $2;

-- name: InsertAssetDeposit :exec
INSERT INTO deposits(deposit_id, tx_hash, out_index, amount,
    confirmation_height, timeout_sweep_pk_script, asset_data)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: UpdateAssetDeposit :exec
UPDATE deposits SET asset_data = $2, confirmation_height = $3
WHERE deposit_id = $1 AND asset_data IS NOT NULL;

-- name: ReserveAssetDeposit :execrows
UPDATE deposits SET asset_request_id = $2, swap_hash = NULL
WHERE deposit_id = $1 AND asset_data IS NOT NULL
    AND asset_request_id IS NULL AND asset_consumed = FALSE;

-- name: BindAssetDeposit :execrows
UPDATE deposits SET swap_hash = $3
WHERE deposit_id = $1 AND asset_request_id = $2
    AND asset_consumed = FALSE AND (swap_hash IS NULL OR swap_hash = $3);

-- name: ReleaseAssetDeposit :execrows
UPDATE deposits SET asset_request_id = NULL, asset_consumed = $3
WHERE deposit_id = $1 AND asset_request_id = $2
    AND (swap_hash = $4 OR (swap_hash IS NULL AND $4 IS NULL));

-- name: RestoreAssetDeposit :exec
UPDATE deposits SET asset_request_id = $2, swap_hash = $3, asset_consumed = $4
WHERE deposit_id = $1 AND asset_data IS NOT NULL;
