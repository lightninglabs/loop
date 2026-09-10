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

-- name: UpdateAssetReservationPurchase :execrows
UPDATE asset_reservations SET
    fee = $2,
    csv_delay = $3,
    required_confirmations = $4,
    execution_delta = $5,
    min_usable_blocks = $6,
    quote = $7,
    max_route_fee_msat = $8,
    main_probe = $9,
    probes_checked_at = $10,
    prepay_route_fee_msat = $11,
    main_route_fee_msat = $12,
    payment_hash = $13,
    paying_node_key = $14,
    payment_request = $15,
    payment_result = $16,
    funding_outpoint = $17,
    confirmation_height = $18,
    prepay_credit = $19,
    deposit_proof = $20,
    skip_probe = $21
WHERE reservation_id = $1;
