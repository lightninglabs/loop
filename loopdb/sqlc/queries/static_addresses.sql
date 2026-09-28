-- name: AllStaticAddresses :many
SELECT * FROM static_addresses
ORDER BY id ASC;

-- name: GetStaticAddress :one
SELECT * FROM static_addresses
WHERE pkscript=$1;

-- name: GetStaticAddressID :one
SELECT id FROM static_addresses
WHERE pkscript=$1;

-- name: CreateStaticAddress :exec
INSERT INTO static_addresses (
    client_pubkey,
    server_pubkey,
    expiry,
    client_key_family,
    client_key_index,
    pkscript,
    protocol_version,
    initiation_height
) VALUES (
             $1,
             $2,
             $3,
             $4,
             $5,
             $6,
             $7,
             $8
         );

-- name: GetLegacyAddress :one
SELECT * FROM static_addresses
ORDER BY id ASC
LIMIT 1;

-- name: ListStaticAddresses :many
SELECT * FROM static_addresses
WHERE id > sqlc.arg(after_id)
ORDER BY id ASC
LIMIT sqlc.arg(page_size);

-- name: GetMaxStaticAddressHtlcKeyIndex :one
SELECT CAST(COALESCE(MAX(htlc_keys.client_key_index), -1) AS INTEGER)
FROM htlc_keys
JOIN static_address_swaps
    ON static_address_swaps.swap_hash = htlc_keys.swap_hash
WHERE htlc_keys.client_key_family = $1;
