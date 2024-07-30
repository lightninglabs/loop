-- name: InsertStaticAddressLoopIn :exec
INSERT INTO static_address_swaps (
    swap_hash,
    swap_invoice,
    last_hop,
    quoted_swap_fee,
    deposit_outpoints,
    htlc_tx,
    htlc_tx_fee_rate,
    htlc_timeout_sweep_tx,
    htlc_timeout_sweep_address
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

-- name: UpdateStaticAddressLoopIn :exec
UPDATE static_address_swaps
SET
    htlc_tx = $2,
    htlc_tx_fee_rate = $3,
    htlc_timeout_sweep_tx = $4,
    htlc_timeout_sweep_address = $5
WHERE
    static_address_swaps.swap_hash = $1;

-- name: InsertStaticAddressMetaUpdate :exec
INSERT INTO static_address_swap_updates (
    swap_hash,
    update_state,
    update_timestamp
) VALUES (
     $1,
     $2,
     $3
 );

-- name: GetStaticAddressLoopInSwap :one
SELECT
    swaps.*,
    static_address_swaps.*,
    htlc_keys.*
FROM
    swaps
        JOIN
    static_address_swaps ON swaps.swap_hash = static_address_swaps.swap_hash
        JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
WHERE
        swaps.swap_hash = $1;

-- name: GetStaticAddressLoopInSwaps :many
SELECT
    swaps.*,
    static_address_swaps.*,
    htlc_keys.*
FROM
    swaps
        JOIN
    static_address_swaps ON swaps.swap_hash = static_address_swaps.swap_hash
        JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
ORDER BY
    swaps.id;

-- name: GetLoopInSwapUpdates :many
SELECT
    static_address_swap_updates.*
FROM
    static_address_swap_updates
WHERE
    static_address_swap_updates.swap_hash = $1;