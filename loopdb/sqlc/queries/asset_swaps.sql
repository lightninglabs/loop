-- name: CreateAssetSwap :exec
    INSERT INTO asset_swaps(
        swap_hash,
        swap_preimage,
        asset_id,
        amt,
        sender_pubkey,
        receiver_pubkey,
        csv_expiry,
        initiation_height,
        created_time,
        server_key_family,
        server_key_index
    )
    VALUES
    (
        $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
    );

-- name: CreateAssetOutSwap :exec
INSERT INTO asset_out_swaps (
    swap_hash
) VALUES (
    $1
);

-- name: UpdateAssetSwapHtlcTx :exec
UPDATE asset_swaps
SET
        htlc_confirmation_height = $2,
        htlc_txid = $3,
        htlc_vout = $4
WHERE
        asset_swaps.swap_hash = $1;

-- name: UpdateAssetSwapOutProof :exec
UPDATE asset_out_swaps
SET 
        raw_proof_file = $2
WHERE
        asset_out_swaps.swap_hash = $1;

-- name: UpdateAssetSwapOutPreimage :exec
UPDATE asset_swaps
SET
        swap_preimage = $2
WHERE
        asset_swaps.swap_hash = $1;

-- name: UpdateAssetSwapSweepTx :exec
UPDATE asset_swaps
SET
        sweep_confirmation_height = $2,
        sweep_txid = $3,
        sweep_pkscript = $4
WHERE
        asset_swaps.swap_hash = $1;

-- name: InsertAssetSwapUpdate :exec
INSERT INTO asset_swaps_updates (
        swap_hash,
        update_state,
        update_timestamp
) VALUES (
        $1,
        $2,
        $3
);


-- name: GetAssetOutSwap :one
SELECT DISTINCT
    sqlc.embed(asw),
    sqlc.embed(aos),
    asu.update_state
FROM
    asset_swaps asw
INNER JOIN (
    SELECT
        swap_hash,
        update_state,
        ROW_NUMBER() OVER(PARTITION BY swap_hash ORDER BY id DESC) as rn
    FROM
        asset_swaps_updates
) asu ON asw.swap_hash = asu.swap_hash AND asu.rn = 1
INNER JOIN asset_out_swaps aos ON asw.swap_hash = aos.swap_hash
WHERE
    asw.swap_hash = $1;

-- name: GetAllAssetOutSwaps :many
SELECT DISTINCT
    sqlc.embed(asw),
    sqlc.embed(aos),
    asu.update_state
FROM
    asset_swaps asw
INNER JOIN (
    SELECT
        swap_hash,
        update_state,
        ROW_NUMBER() OVER(PARTITION BY swap_hash ORDER BY id DESC) as rn
    FROM
        asset_swaps_updates
) asu ON asw.swap_hash = asu.swap_hash AND asu.rn = 1
INNER JOIN asset_out_swaps aos ON asw.swap_hash = aos.swap_hash
ORDER BY
    asw.id;

