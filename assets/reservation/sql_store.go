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
	"google.golang.org/protobuf/proto"
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

			if err := updatePurchase(ctx, q, &next); err != nil {
				return err
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
			if err := preservePurchase(r, stored); err != nil {
				return err
			}
			if sameProgress(r, stored) {
				next = *stored
				return nil
			}
			next.CreatedAt = stored.CreatedAt
			if err := updatePurchase(ctx, q, &next); err != nil {
				return err
			}
			return insertUpdate(ctx, q, &next)
		})
	if err == nil {
		*r = next
	}
	return err
}

// GetReservation reads the purchase facts and latest state in one statement,
// so PostgreSQL cannot mix snapshots from concurrent worker updates.
func (s *SqlStore) GetReservation(ctx context.Context, id ID) (*Reservation,
	error) {

	return loadReservation(ctx, s.db.Queries, id)
}

// GetReservations filters and reads the reservation and its latest state in
// one statement, giving both SQLite and PostgreSQL a consistent snapshot.
func (s *SqlStore) GetReservations(ctx context.Context, filter StateFilter) (
	[]*Reservation, error) {

	if err := filter.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.GetAssetReservations(ctx, sqlc.GetAssetReservationsParams{
		State: sql.NullString{
			String: string(filter.State), Valid: filter.State != "",
		},
		ActiveOnly:    filter.ActiveOnly,
		CanceledState: string(Canceled),
		RejectedState: string(QuoteRejected),
		FailedState:   string(QuoteFailed),
		ExpiredState:  string(Expired),
	})
	if err != nil {
		return nil, err
	}
	var result []*Reservation
	for _, row := range rows {
		if !row.UpdateState.Valid || !row.UpdateTimestamp.Valid {
			return nil, errors.New("missing reservation state history")
		}
		r, err := toReservation(row.AssetReservation,
			row.UpdateState.String, row.UpdateTimestamp.Time)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
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

	row, err := q.GetAssetReservationSnapshot(ctx, id[:])
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !row.UpdateState.Valid || !row.UpdateTimestamp.Valid {
		return nil, errors.New("missing reservation state history")
	}
	return toReservation(row.AssetReservation, row.UpdateState.String,
		row.UpdateTimestamp.Time)
}

func toReservation(row sqlc.AssetReservation, state string,
	updatedAt time.Time) (*Reservation, error) {

	if len(row.ReservationID) != 32 || len(row.AssetID) != 32 ||
		len(row.ClientPubkey) != btcec.PubKeyBytesLenCompressed ||
		state == "" || row.ClientKeyIndex < 0 ||
		row.ClientKeyIndex > math.MaxUint32 {

		return nil, errors.New("invalid stored asset reservation")
	}
	pubkey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, err
	}
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
		State:     fsm.StateType(state),
		CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: updatedAt.UTC(),
	}
	if err := readPurchase(row, r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

func sameRequest(a, b *Reservation) bool {
	return a.ID == b.ID && a.AssetID == b.AssetID && a.Amount == b.Amount &&
		(a.Fee == 0 || b.Fee == 0 || a.Terms == b.Terms) &&
		a.ClientKey.KeyLocator == b.ClientKey.KeyLocator &&
		a.ClientKey.PubKey.IsEqual(b.ClientKey.PubKey)
}

func sameProgress(a, b *Reservation) bool {
	return a.State == b.State && a.Terms == b.Terms && a.Probes == b.Probes &&
		a.SkipProbe == b.SkipProbe &&
		proto.Equal(a.ProbeResult, b.ProbeResult) &&
		a.MaxRouteFeeMsat == b.MaxRouteFeeMsat &&
		proto.Equal(a.PaymentResult, b.PaymentResult) &&
		preservePurchase(a, b) == nil && preservePurchase(b, a) == nil
}
