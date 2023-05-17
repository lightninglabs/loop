-- name: GetLoopOutSwaps :many
SELECT 
    swaps.*,
    loopout_swaps.*,
    htlc_keys.*
FROM 
    swaps
JOIN
    loopout_swaps ON swaps.swap_hash = loopout_swaps.swap_hash
JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
ORDER BY
    swaps.id;

-- name: GetLoopOutSwap :one
SELECT 
    swaps.*,
    loopout_swaps.*,
    htlc_keys.*
FROM
    swaps
JOIN
    loopout_swaps ON swaps.swap_hash = loopout_swaps.swap_hash
JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
WHERE
    swaps.swap_hash = $1;

-- name: GetLoopInSwaps :many
SELECT 
    swaps.*,
    loopin_swaps.*,
    htlc_keys.*
FROM
    swaps
JOIN
    loopin_swaps ON swaps.swap_hash = loopin_swaps.swap_hash
JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
ORDER BY
    swaps.id;

-- name: GetLoopInSwap :one
SELECT 
    swaps.*,
    loopin_swaps.*,
    htlc_keys.*
FROM
    swaps
JOIN
    loopin_swaps ON swaps.swap_hash = loopin_swaps.swap_hash
JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
WHERE
    swaps.swap_hash = $1;

-- name: GetSwapUpdates :many
SELECT 
    *
FROM
    swap_updates
WHERE
    swap_hash = $1
ORDER BY
    id ASC;

-- name: InsertSwap :exec
INSERT INTO swaps (
    swap_hash,
    preimage,
    initiation_time,
    amount_requested,
    cltv_expiry,
    max_miner_fee,
    max_swap_fee,
    initiation_height,
    protocol_version,
    label
) VALUES (
     $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: InsertSwapUpdate :exec
INSERT INTO swap_updates (
    swap_hash,
    update_timestamp,
    update_state,
    htlc_txhash,
    server_cost,
    onchain_cost,
    offchain_cost
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: InsertLoopOut :exec
INSERT INTO loopout_swaps (
    swap_hash,
    dest_address,
    swap_invoice,
    max_swap_routing_fee,
    sweep_conf_target,
    htlc_confirmations,
    outgoing_chan_set,
    prepay_invoice,
    max_prepay_routing_fee,
    publication_deadline
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: InsertLoopIn :exec
INSERT INTO loopin_swaps (
    swap_hash,
    htlc_conf_target,
    last_hop,
    external_htlc
) VALUES (
    $1, $2, $3, $4
);

-- name: InsertHtlcKeys :exec
INSERT INTO htlc_keys(
    swap_hash,
    sender_script_pubkey,
    receiver_script_pubkey,
    sender_internal_pubkey,
    receiver_internal_pubkey,
    client_key_family,
    client_key_index
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);