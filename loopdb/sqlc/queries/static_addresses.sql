-- name: AllStaticAddresses :many
SELECT * FROM static_addresses;

-- name: GetStaticAddress :one
SELECT * FROM static_addresses
WHERE pkscript=$1;

-- name: CreateStaticAddress :exec
INSERT INTO static_addresses (
    client_pubkey,
    server_pubkey,
    expiry,
    client_key_family,
    client_key_index,
    pkscript,
    protocol_version
) VALUES (
             $1,
             $2,
             $3,
             $4,
             $5,
             $6,
             $7
         );