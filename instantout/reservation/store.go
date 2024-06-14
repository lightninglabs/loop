package reservation

import (
	"context"
	"database/sql"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
)

// BaseDB is the interface that contains all the queries generated
// by sqlc for the reservation table.
type BaseDB interface {
	// CreateReservation stores the reservation in the database.
	CreateReservation(ctx context.Context,
		arg sqlc.CreateReservationParams) error

	// GetReservation retrieves the reservation from the database.
	GetReservation(ctx context.Context,
		reservationID []byte) (sqlc.Reservation, error)

	// GetReservationUpdates fetches all updates for a reservation.
	GetReservationUpdates(ctx context.Context,
		reservationID []byte) ([]sqlc.ReservationUpdate, error)

	// GetReservations lists all existing reservations the client has ever
	// made.
	GetReservations(ctx context.Context) ([]sqlc.Reservation, error)

	// UpdateReservation inserts a new reservation update.
	UpdateReservation(ctx context.Context,
		arg sqlc.UpdateReservationParams) error

	// ExecTx allows for executing a function in the context of a database
	// transaction.
	ExecTx(ctx context.Context, txOptions loopdb.TxOptions,
		txBody func(*sqlc.Queries) error) error
}

// SQLStore manages the reservations in the database.
type SQLStore struct {
	baseDb BaseDB

	clock clock.Clock
}

// NewSQLStore creates a new SQLStore.
func NewSQLStore(db BaseDB) *SQLStore {
	return &SQLStore{
		baseDb: db,
		clock:  clock.NewDefaultClock(),
	}
}

// CreateReservation stores the reservation in the database.
func (r *SQLStore) CreateReservation(ctx context.Context,
	reservation *Reservation) error {

	args := sqlc.CreateReservationParams{
		ReservationID:    reservation.ID[:],
		ClientPubkey:     reservation.ClientPubkey.SerializeCompressed(),
		ServerPubkey:     reservation.ServerPubkey.SerializeCompressed(),
		Expiry:           int32(reservation.Expiry),
		Value:            int64(reservation.Value),
		ClientKeyFamily:  int32(reservation.KeyLocator.Family),
		ClientKeyIndex:   int32(reservation.KeyLocator.Index),
		InitiationHeight: reservation.InitiationHeight,
	}

	updateArgs := sqlc.InsertReservationUpdateParams{
		ReservationID:   reservation.ID[:],
		UpdateTimestamp: r.clock.Now().UTC(),
		UpdateState:     string(reservation.State),
	}

	return r.baseDb.ExecTx(ctx, loopdb.NewSqlWriteOpts(),
		func(q *sqlc.Queries) error {
			err := q.CreateReservation(ctx, args)
			if err != nil {
				return err
			}

			return q.InsertReservationUpdate(ctx, updateArgs)
		})
}

// UpdateReservation updates the reservation in the database.
func (r *SQLStore) UpdateReservation(ctx context.Context,
	reservation *Reservation) error {

	var txHash []byte
	var outIndex sql.NullInt32
	if reservation.Outpoint != nil {
		txHash = reservation.Outpoint.Hash[:]
		outIndex = sql.NullInt32{
			Int32: int32(reservation.Outpoint.Index),
			Valid: true,
		}
	}

	insertUpdateArgs := sqlc.InsertReservationUpdateParams{
		ReservationID:   reservation.ID[:],
		UpdateTimestamp: r.clock.Now().UTC(),
		UpdateState:     string(reservation.State),
	}

	updateArgs := sqlc.UpdateReservationParams{
		ReservationID: reservation.ID[:],
		TxHash:        txHash,
		OutIndex:      outIndex,
		ConfirmationHeight: marshalSqlNullInt32(
			int32(reservation.ConfirmationHeight),
		),
	}

	return r.baseDb.ExecTx(ctx, loopdb.NewSqlWriteOpts(),
		func(q *sqlc.Queries) error {
			err := q.UpdateReservation(ctx, updateArgs)
			if err != nil {
				return err
			}

			return q.InsertReservationUpdate(ctx, insertUpdateArgs)
		})
}

// GetReservation retrieves the reservation from the database.
func (r *SQLStore) GetReservation(ctx context.Context,
	reservationId ID) (*Reservation, error) {

	var reservation *Reservation
	err := r.baseDb.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			var err error
			reservationRow, err := q.GetReservation(
				ctx, reservationId[:],
			)
			if err != nil {
				return err
			}

			reservationUpdates, err := q.GetReservationUpdates(
				ctx, reservationId[:],
			)
			if err != nil {
				return err
			}

			if len(reservationUpdates) == 0 {
				return errors.New("no reservation updates")
			}

			reservation, err = sqlReservationToReservation(
				reservationRow,
				reservationUpdates[len(reservationUpdates)-1],
			)
			if err != nil {
				return err
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return reservation, nil
}

// ListReservations lists all existing reservations the client has ever made.
func (r *SQLStore) ListReservations(ctx context.Context) ([]*Reservation,
	error) {

	var result []*Reservation

	err := r.baseDb.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			var err error

			reservations, err := q.GetReservations(ctx)
			if err != nil {
				return err
			}

			for _, reservation := range reservations {
				reservationUpdates, err := q.GetReservationUpdates(
					ctx, reservation.ReservationID,
				)
				if err != nil {
					return err
				}

				if len(reservationUpdates) == 0 {
					return errors.New(
						"no reservation updates",
					)
				}

				res, err := sqlReservationToReservation(
					reservation, reservationUpdates[len(
						reservationUpdates,
					)-1],
				)
				if err != nil {
					return err
				}

				result = append(result, res)
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// sqlReservationToReservation converts a sql reservation to a reservation.
func sqlReservationToReservation(row sqlc.Reservation,
	lastUpdate sqlc.ReservationUpdate) (*Reservation,
	error) {

	id := ID{}
	err := id.FromByteSlice(row.ReservationID)
	if err != nil {
		return nil, err
	}

	clientPubkey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, err
	}

	serverPubkey, err := btcec.ParsePubKey(row.ServerPubkey)
	if err != nil {
		return nil, err
	}

	var txHash *chainhash.Hash
	if row.TxHash != nil {
		txHash, err = chainhash.NewHash(row.TxHash)
		if err != nil {
			return nil, err
		}
	}

	var outpoint *wire.OutPoint
	if row.OutIndex.Valid {
		outpoint = wire.NewOutPoint(
			txHash, uint32(unmarshalSqlNullInt32(row.OutIndex)),
		)
	}

	return &Reservation{
		ID:           id,
		ClientPubkey: clientPubkey,
		ServerPubkey: serverPubkey,
		Expiry:       uint32(row.Expiry),
		Value:        btcutil.Amount(row.Value),
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.ClientKeyFamily),
			Index:  uint32(row.ClientKeyIndex),
		},
		Outpoint: outpoint,
		ConfirmationHeight: uint32(
			unmarshalSqlNullInt32(row.ConfirmationHeight),
		),
		InitiationHeight: row.InitiationHeight,
		State:            fsm.StateType(lastUpdate.UpdateState),
	}, nil
}

// marshalSqlNullInt32 converts an int32 to a sql.NullInt32.
func marshalSqlNullInt32(i int32) sql.NullInt32 {
	return sql.NullInt32{
		Int32: i,
		Valid: i != 0,
	}
}

// unmarshalSqlNullInt32 converts a sql.NullInt32 to an int32.
func unmarshalSqlNullInt32(i sql.NullInt32) int32 {
	if i.Valid {
		return i.Int32
	}

	return 0
}
