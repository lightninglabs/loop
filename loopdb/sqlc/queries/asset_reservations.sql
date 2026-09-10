-- name: CreateAssetReservation :execrows
INSERT INTO asset_reservations (
    reservation_id,
    asset_id,
    amount,
    fee,
    csv_delay,
    required_confirmations,
    execution_delta,
    client_pubkey,
    client_key_family,
    client_key_index,
    created_at, group_key
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
    $11, $12
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
    quote = $6,
    max_route_fee_msat = $7,
    main_probe = $8,
    probes_checked_at = $9,
    prepay_route_fee_msat = $10,
    main_route_fee_msat = $11,
    payment_hash = $12,
    paying_node_key = $13,
    payment_request = $14,
    payment_result = $15,
    funding_outpoint = $16,
    confirmation_height = $17,
    prepay_credit = $18,
    deposit_proof = $19,
    skip_probe = $20,
    probe_node_key = $21,
    probe_request = $22,
    probe_result = $23,
    probe_deadline = $24,
    probe_fee_known = $25
WHERE reservation_id = $1;
