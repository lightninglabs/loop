-- name: CreateAssetReservation :execrows
INSERT INTO asset_reservations (
    reservation_id,
    asset_id,
    amount,
    fee,
    csv_delay,
    required_confirmations,
    execution_delta,
    min_usable_blocks,
    client_pubkey,
    client_key_family,
    client_key_index,
    created_at
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
) ON CONFLICT (reservation_id) DO NOTHING;

-- name: GetAssetReservation :one
SELECT * FROM asset_reservations WHERE reservation_id = $1;

-- name: GetAssetReservations :many
SELECT * FROM asset_reservations ORDER BY id;

-- name: InsertAssetReservationUpdate :exec
INSERT INTO asset_reservation_updates (
    reservation_id, update_state, update_timestamp
) VALUES ($1, $2, $3);

-- name: GetAssetReservationUpdates :many
SELECT * FROM asset_reservation_updates
WHERE reservation_id = $1
ORDER BY id;
