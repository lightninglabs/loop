-- name: InsertInstantOut :exec
INSERT INTO instantout_swaps (
        swap_hash,
        preimage, 
        sweep_address,
        outgoing_chan_set,
        htlc_fee_rate,
        reservation_ids,
        swap_invoice
) VALUES (
        $1,
        $2,
        $3,
        $4,
        $5,
        $6,
        $7
);

-- name: UpdateInstantOut :exec
UPDATE instantout_swaps
SET
        finalized_htlc_tx = $2,
        sweep_tx_hash = $3,
        finalized_sweepless_sweep_tx = $4,
        sweep_confirmation_height = $5
WHERE
        instantout_swaps.swap_hash = $1;

-- name: InsertInstantOutUpdate :exec
INSERT INTO instantout_updates (
        swap_hash,
        update_state,
        update_timestamp
) VALUES (
        $1,
        $2,
        $3
);

-- name: GetInstantOutSwap :one
SELECT
    swaps.*,
    instantout_swaps.*,
    htlc_keys.*
FROM
    swaps
JOIN
    instantout_swaps ON swaps.swap_hash = instantout_swaps.swap_hash
JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
WHERE
    swaps.swap_hash = $1;

-- name: GetInstantOutSwaps :many
SELECT 
    swaps.*,
    instantout_swaps.*,
    htlc_keys.*
FROM
    swaps
JOIN
    instantout_swaps ON swaps.swap_hash = instantout_swaps.swap_hash
JOIN
    htlc_keys ON swaps.swap_hash = htlc_keys.swap_hash
ORDER BY
    swaps.id;

-- name: GetInstantOutSwapUpdates :many
SELECT
    instantout_updates.*
FROM
    instantout_updates
WHERE
    instantout_updates.swap_hash = $1;
