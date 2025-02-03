-- name: CreateReservation :exec
INSERT INTO reservations (
    reservation_id,
    client_pubkey,
    server_pubkey,
    expiry,
    value,
    client_key_family,
    client_key_index,
    initiation_height,
    protocol_version,
    prepay_invoice
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
    $10
);

-- name: UpdateReservation :exec
UPDATE reservations
SET
    tx_hash = $2,
    out_index = $3,
    confirmation_height = $4
WHERE
    reservations.reservation_id = $1;

-- name: InsertReservationUpdate :exec
INSERT INTO reservation_updates (
    reservation_id,
    update_state,
    update_timestamp
) VALUES (
    $1,
    $2,
    $3
);

-- name: GetReservation :one
SELECT
    *
FROM
    reservations
WHERE
    reservation_id = $1;

-- name: GetReservations :many
SELECT
    *
FROM
    reservations
ORDER BY
    id ASC;

-- name: GetReservationUpdates :many
SELECT
    reservation_updates.*
FROM
    reservation_updates
WHERE
    reservation_id = $1
ORDER BY
    id ASC;