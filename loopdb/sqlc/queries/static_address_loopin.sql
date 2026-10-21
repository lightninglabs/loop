-- name: InsertStaticAddressLoopIn :exec
INSERT INTO static_address_swaps (
    swap_hash,
    swap_invoice,
    last_hop,
    payment_timeout_seconds,
    quoted_swap_fee_satoshis,
    deposit_outpoints,
    selected_amount,
    htlc_tx_fee_rate_sat_kw,
    htlc_timeout_sweep_tx_id,
    htlc_timeout_sweep_address,
    fast
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6,
    $7,
    $8,
    $9,
    $10,
    $11
);

-- name: UpdateStaticAddressLoopIn :exec
UPDATE static_address_swaps
SET
    htlc_tx_fee_rate_sat_kw = $2,
    htlc_timeout_sweep_tx_id = $3
WHERE
    swap_hash = $1;

-- name: RecordStaticAddressRiskDecision :exec
UPDATE static_address_swaps
SET
    confirmation_risk_decision = $2,
    confirmation_risk_decision_time = CASE
        WHEN confirmation_risk_decision = $2 THEN
            COALESCE(confirmation_risk_decision_time, $3)
        ELSE $3
    END
WHERE
    swap_hash = $1
    AND NOT (
        confirmation_risk_decision = 'rejected'
        AND $2 = 'accepted'
    );

-- name: InsertStaticAddressMetaUpdate :one
INSERT INTO static_address_swap_updates (
    swap_hash,
    update_state,
    update_timestamp
) VALUES (
     $1,
     $2,
     $3
 )
RETURNING id;

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
        -- This subquery picks the latest update for each swap_hash by id, not
        -- by timestamp, since a later update can have an earlier timestamp,
        -- for example after the clock was set back.
        AND u.id = (
            SELECT MAX(id)
            FROM static_address_swap_updates
            WHERE swap_hash = u.swap_hash
        )
WHERE
        (',' || $1 || ',') LIKE ('%,' || u.update_state || ',%')
ORDER BY
    swaps.id;

-- name: GetLoopInSwapUpdates :many
-- The last update is the swap's current state.
SELECT
    static_address_swap_updates.*
FROM
    static_address_swap_updates
WHERE
    swap_hash = $1
ORDER BY
    id ASC;

-- name: IsStored :one
SELECT EXISTS (
    SELECT 1
    FROM static_address_swaps
    WHERE swap_hash = $1
);

-- name: OverrideSelectedSwapAmount :exec
UPDATE static_address_swaps
SET
    selected_amount = $2
WHERE swap_hash = $1;

-- name: MapDepositToSwap :exec
UPDATE
    deposits
SET
    swap_hash = $2
WHERE
    deposit_id = $1;

-- name: SwapHashForDepositID :one
SELECT
    swap_hash
FROM
    deposits
WHERE
    deposit_id = $1;

-- name: DepositIDsForSwapHash :many
SELECT
    deposit_id
FROM
    deposits
WHERE
    swap_hash = $1;

-- name: DepositsForSwapHash :many
SELECT
    d.*,
    u.update_state,
    u.update_timestamp
FROM
    deposits d
        LEFT JOIN
    deposit_updates u ON u.id = (
        SELECT id
        FROM deposit_updates
        WHERE deposit_id = d.deposit_id
        ORDER BY update_timestamp DESC
        LIMIT 1
    )
WHERE
    d.swap_hash = $1;


