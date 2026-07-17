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
    initiation_height,
    label
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

-- name: GetLegacyAddress :one
SELECT * FROM static_addresses
ORDER BY id ASC
LIMIT 1;

-- name: ListStaticAddresses :many
SELECT * FROM static_addresses
WHERE id > sqlc.arg(after_id)
ORDER BY id ASC
LIMIT sqlc.arg(page_size);
-- name: UpdateStaticAddressLabel :execrows
UPDATE static_addresses
SET label = $2
WHERE pkscript = $1;
