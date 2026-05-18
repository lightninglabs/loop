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
    fast,
    change_static_address_id
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
    $11,
    $12
);

-- name: UpdateStaticAddressLoopIn :exec
UPDATE static_address_swaps
SET
    htlc_tx_fee_rate_sat_kw = $2,
    htlc_timeout_sweep_tx_id = $3,
    confirmed_htlc_tx_id = $4,
    confirmed_htlc_output_index = $5,
    confirmed_htlc_output_value = $6
WHERE
    swap_hash = $1;

-- name: RecordStaticAddressRiskDecision :exec
UPDATE static_address_swaps
SET
    confirmation_risk_decision = $2,
    confirmation_risk_decision_time = $3
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
    htlc_keys.*,
    change_address.client_pubkey     change_client_pubkey,
    change_address.server_pubkey     change_server_pubkey,
    change_address.expiry            change_expiry,
    change_address.client_key_family change_client_key_family,
    change_address.client_key_index  change_client_key_index,
    change_address.pkscript          change_pkscript,
    change_address.protocol_version  change_protocol_version,
    change_address.initiation_height change_initiation_height
FROM
    swaps
        JOIN
    static_address_swaps ON swaps.swap_hash = static_address_swaps.swap_hash
        JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
        LEFT JOIN
    static_addresses change_address
        ON static_address_swaps.change_static_address_id = change_address.id
WHERE
        swaps.swap_hash = $1;

-- name: GetStaticAddressLoopInSwapsByStates :many
SELECT
    swaps.*,
    static_address_swaps.*,
    htlc_keys.*,
    change_address.client_pubkey     change_client_pubkey,
    change_address.server_pubkey     change_server_pubkey,
    change_address.expiry            change_expiry,
    change_address.client_key_family change_client_key_family,
    change_address.client_key_index  change_client_key_index,
    change_address.pkscript          change_pkscript,
    change_address.protocol_version  change_protocol_version,
    change_address.initiation_height change_initiation_height
FROM
    swaps
        JOIN
    static_address_swaps ON swaps.swap_hash = static_address_swaps.swap_hash
        JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
        LEFT JOIN
    static_addresses change_address
        ON static_address_swaps.change_static_address_id = change_address.id
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
    sa.client_pubkey     client_pubkey,
    sa.server_pubkey     server_pubkey,
    sa.expiry            expiry,
    sa.client_key_family client_key_family,
    sa.client_key_index  client_key_index,
    sa.pkscript          pkscript,
    sa.protocol_version  protocol_version,
    sa.initiation_height initiation_height,
    u.update_state,
    u.update_timestamp
FROM
    deposits d
        LEFT JOIN static_addresses sa ON sa.id = d.static_address_id
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



