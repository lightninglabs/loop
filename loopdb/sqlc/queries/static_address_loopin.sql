-- name: InsertStaticAddressLoopIn :exec
INSERT INTO static_address_swaps (
    swap_hash,
    swap_invoice,
    last_hop,
    payment_timeout_seconds,
    quoted_swap_fee_satoshis,
    deposit_outpoints,
    htlc_tx_fee_rate_sat_kw,
    htlc_timeout_sweep_tx_id,
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
    htlc_tx_fee_rate_sat_kw = $2,
    htlc_timeout_sweep_tx_id = $3
WHERE
    swap_hash = $1;

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

-- name: GetStaticAddressLoopInSwapsByStates :many
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
        JOIN
    static_address_swap_updates u ON swaps.swap_hash = u.swap_hash
        -- This subquery ensures that we are checking only the latest update for
        -- each swap_hash.
        AND u.update_timestamp = (
            SELECT MAX(update_timestamp)
            FROM static_address_swap_updates
            WHERE swap_hash = u.swap_hash
        )
WHERE
        (',' || $1 || ',') LIKE ('%,' || u.update_state || ',%')
ORDER BY
    swaps.id;

-- name: GetLoopInSwapUpdates :many
SELECT
    static_address_swap_updates.*
FROM
    static_address_swap_updates
WHERE
    swap_hash = $1;

-- name: IsStored :one
SELECT EXISTS (
    SELECT 1
    FROM static_address_swaps
    WHERE swap_hash = $1
);
