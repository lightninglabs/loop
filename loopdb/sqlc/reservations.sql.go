// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.25.0
// source: reservations.sql

package sqlc

import (
	"context"
	"database/sql"
	"time"
)

const createReservation = `-- name: CreateReservation :exec
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
)
`

type CreateReservationParams struct {
	ReservationID    []byte
	ClientPubkey     []byte
	ServerPubkey     []byte
	Expiry           int32
	Value            int64
	ClientKeyFamily  int32
	ClientKeyIndex   int32
	InitiationHeight int32
	ProtocolVersion  int32
	PrepayInvoice    string
}

func (q *Queries) CreateReservation(ctx context.Context, arg CreateReservationParams) error {
	_, err := q.db.ExecContext(ctx, createReservation,
		arg.ReservationID,
		arg.ClientPubkey,
		arg.ServerPubkey,
		arg.Expiry,
		arg.Value,
		arg.ClientKeyFamily,
		arg.ClientKeyIndex,
		arg.InitiationHeight,
		arg.ProtocolVersion,
		arg.PrepayInvoice,
	)
	return err
}

const getReservation = `-- name: GetReservation :one
SELECT
    id, reservation_id, client_pubkey, server_pubkey, expiry, value, client_key_family, client_key_index, initiation_height, tx_hash, out_index, confirmation_height, protocol_version, prepay_invoice
FROM
    reservations
WHERE
    reservation_id = $1
`

func (q *Queries) GetReservation(ctx context.Context, reservationID []byte) (Reservation, error) {
	row := q.db.QueryRowContext(ctx, getReservation, reservationID)
	var i Reservation
	err := row.Scan(
		&i.ID,
		&i.ReservationID,
		&i.ClientPubkey,
		&i.ServerPubkey,
		&i.Expiry,
		&i.Value,
		&i.ClientKeyFamily,
		&i.ClientKeyIndex,
		&i.InitiationHeight,
		&i.TxHash,
		&i.OutIndex,
		&i.ConfirmationHeight,
		&i.ProtocolVersion,
		&i.PrepayInvoice,
	)
	return i, err
}

const getReservationUpdates = `-- name: GetReservationUpdates :many
SELECT
    reservation_updates.id, reservation_updates.reservation_id, reservation_updates.update_state, reservation_updates.update_timestamp
FROM
    reservation_updates
WHERE
    reservation_id = $1
ORDER BY
    id ASC
`

func (q *Queries) GetReservationUpdates(ctx context.Context, reservationID []byte) ([]ReservationUpdate, error) {
	rows, err := q.db.QueryContext(ctx, getReservationUpdates, reservationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ReservationUpdate
	for rows.Next() {
		var i ReservationUpdate
		if err := rows.Scan(
			&i.ID,
			&i.ReservationID,
			&i.UpdateState,
			&i.UpdateTimestamp,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getReservations = `-- name: GetReservations :many
SELECT
    id, reservation_id, client_pubkey, server_pubkey, expiry, value, client_key_family, client_key_index, initiation_height, tx_hash, out_index, confirmation_height, protocol_version, prepay_invoice
FROM
    reservations
ORDER BY
    id ASC
`

func (q *Queries) GetReservations(ctx context.Context) ([]Reservation, error) {
	rows, err := q.db.QueryContext(ctx, getReservations)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Reservation
	for rows.Next() {
		var i Reservation
		if err := rows.Scan(
			&i.ID,
			&i.ReservationID,
			&i.ClientPubkey,
			&i.ServerPubkey,
			&i.Expiry,
			&i.Value,
			&i.ClientKeyFamily,
			&i.ClientKeyIndex,
			&i.InitiationHeight,
			&i.TxHash,
			&i.OutIndex,
			&i.ConfirmationHeight,
			&i.ProtocolVersion,
			&i.PrepayInvoice,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const insertReservationUpdate = `-- name: InsertReservationUpdate :exec
INSERT INTO reservation_updates (
    reservation_id,
    update_state,
    update_timestamp
) VALUES (
    $1,
    $2,
    $3
)
`

type InsertReservationUpdateParams struct {
	ReservationID   []byte
	UpdateState     string
	UpdateTimestamp time.Time
}

func (q *Queries) InsertReservationUpdate(ctx context.Context, arg InsertReservationUpdateParams) error {
	_, err := q.db.ExecContext(ctx, insertReservationUpdate, arg.ReservationID, arg.UpdateState, arg.UpdateTimestamp)
	return err
}

const updateReservation = `-- name: UpdateReservation :exec
UPDATE reservations
SET
    tx_hash = $2,
    out_index = $3,
    confirmation_height = $4
WHERE
    reservations.reservation_id = $1
`

type UpdateReservationParams struct {
	ReservationID      []byte
	TxHash             []byte
	OutIndex           sql.NullInt32
	ConfirmationHeight sql.NullInt32
}

func (q *Queries) UpdateReservation(ctx context.Context, arg UpdateReservationParams) error {
	_, err := q.db.ExecContext(ctx, updateReservation,
		arg.ReservationID,
		arg.TxHash,
		arg.OutIndex,
		arg.ConfirmationHeight,
	)
	return err
}
