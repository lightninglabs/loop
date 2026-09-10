package reservation

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
)

// SqlStore stores client reservations in SQLite or PostgreSQL.
type SqlStore struct {
	db    *loopdb.BaseDB
	clock clock.Clock
}

var _ Store = (*SqlStore)(nil)

// NewSqlStore returns a reservation store using the client's existing database.
func NewSqlStore(db *loopdb.BaseDB) *SqlStore {
	return &SqlStore{
		db:    db,
		clock: clock.NewDefaultClock(),
	}
}

// CreateReservation saves agreed terms and the first state together. An
// identical request returns the saved record, including its current progress.
func (s *SqlStore) CreateReservation(ctx context.Context, r *Reservation) error {
	if err := r.Validate(); err != nil {
		return err
	}

	next := *r
	next.CreatedAt = s.clock.Now().UTC().Truncate(time.Microsecond)
	next.UpdatedAt = next.CreatedAt
	err := s.db.ExecTx(ctx, &loopdb.SqliteTxOptions{},
		func(q *sqlc.Queries) error {
			count, err := q.CreateAssetReservation(ctx,
				sqlc.CreateAssetReservationParams{
					ReservationID:         r.ID[:],
					AssetID:               r.AssetID[:],
					Amount:                int64(r.Amount),
					Fee:                   int64(r.Fee),
					CsvDelay:              int32(r.CSVDelay),
					RequiredConfirmations: int32(r.RequiredConfirmations),
					ExecutionDelta:        int32(r.ExecutionDelta),
					MinUsableBlocks:       int32(r.MinUsableBlocks),
					ClientPubkey:          r.ClientKey.PubKey.SerializeCompressed(),
					ClientKeyFamily:       int32(r.ClientKey.Family),
					ClientKeyIndex:        int64(r.ClientKey.Index),
					CreatedAt:             next.CreatedAt,
				},
			)
			if err != nil {
				return err
			}
			if count == 0 {
				stored, err := loadReservation(ctx, q, r.ID)
				if err != nil {
					return err
				}
				if !sameRequest(r, stored) {
					return ErrRequestConflict
				}
				next = *stored
				return nil
			}

			return insertUpdate(ctx, q, &next)
		})
	if err == nil {
		*r = next
	}
	return err
}

// UpdateReservation saves progress without changing the agreed terms. A
// failed transaction leaves the caller's timestamps unchanged.
func (s *SqlStore) UpdateReservation(ctx context.Context, r *Reservation) error {
	if err := r.Validate(); err != nil {
		return err
	}
	next := *r
	next.UpdatedAt = s.clock.Now().UTC().Truncate(time.Microsecond)
	err := s.db.ExecTx(ctx, &loopdb.SqliteTxOptions{},
		func(q *sqlc.Queries) error {
			stored, err := loadReservation(ctx, q, r.ID)
			if err != nil {
				return err
			}
			if !sameRequest(r, stored) {
				return ErrRequestConflict
			}
			next.CreatedAt = stored.CreatedAt
			return insertUpdate(ctx, q, &next)
		})
	if err == nil {
		*r = next
	}
	return err
}

// GetReservation loads the agreed terms and latest state in one transaction.
func (s *SqlStore) GetReservation(ctx context.Context, id ID) (*Reservation,
	error) {

	var result *Reservation
	err := s.db.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			var err error
			result, err = loadReservation(ctx, q, id)
			return err
		})
	return result, err
}

// GetReservations loads saved records for status and manager recovery.
func (s *SqlStore) GetReservations(ctx context.Context) ([]*Reservation, error) {
	var result []*Reservation
	err := s.db.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			rows, err := q.GetAssetReservations(ctx)
			if err != nil {
				return err
			}
			for _, row := range rows {
				updates, err := q.GetAssetReservationUpdates(
					ctx, row.ReservationID,
				)
				if err != nil {
					return err
				}
				r, err := toReservation(row, updates)
				if err != nil {
					return err
				}
				result = append(result, r)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func insertUpdate(ctx context.Context, q *sqlc.Queries, r *Reservation) error {
	return q.InsertAssetReservationUpdate(ctx,
		sqlc.InsertAssetReservationUpdateParams{
			ReservationID:   r.ID[:],
			UpdateState:     string(r.State),
			UpdateTimestamp: r.UpdatedAt,
		},
	)
}

func loadReservation(ctx context.Context, q *sqlc.Queries, id ID) (*Reservation,
	error) {

	row, err := q.GetAssetReservation(ctx, id[:])
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	updates, err := q.GetAssetReservationUpdates(ctx, id[:])
	if err != nil {
		return nil, err
	}
	return toReservation(row, updates)
}

func toReservation(row sqlc.AssetReservation,
	updates []sqlc.AssetReservationUpdate) (*Reservation, error) {

	if len(row.ReservationID) != 32 || len(row.AssetID) != 32 ||
		len(row.ClientPubkey) != btcec.PubKeyBytesLenCompressed ||
		len(updates) == 0 || row.ClientKeyIndex < 0 ||
		row.ClientKeyIndex > math.MaxUint32 {

		return nil, errors.New("invalid stored asset reservation")
	}
	pubkey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, err
	}
	latest := updates[len(updates)-1]
	r := &Reservation{
		ID: ID(row.ReservationID),
		Terms: Terms{
			AssetID:               [32]byte(row.AssetID),
			Amount:                uint64(row.Amount),
			Fee:                   uint64(row.Fee),
			CSVDelay:              uint32(row.CsvDelay),
			RequiredConfirmations: uint32(row.RequiredConfirmations),
			ExecutionDelta:        uint32(row.ExecutionDelta),
			MinUsableBlocks:       uint32(row.MinUsableBlocks),
		},
		ClientKey: keychain.KeyDescriptor{
			PubKey: pubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(row.ClientKeyFamily),
				Index:  uint32(row.ClientKeyIndex),
			},
		},
		State:     fsm.StateType(latest.UpdateState),
		CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: latest.UpdateTimestamp.UTC(),
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

func sameRequest(a, b *Reservation) bool {
	return a.ID == b.ID && a.Terms == b.Terms &&
		a.ClientKey.KeyLocator == b.ClientKey.KeyLocator &&
		a.ClientKey.PubKey.IsEqual(b.ClientKey.PubKey)
}
