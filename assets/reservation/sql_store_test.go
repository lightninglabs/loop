package reservation

import (
	"context"
	"database/sql"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// reservationQueryCounter checks that loading state and funding facts does
// not require separate statements, which could observe different snapshots.
type reservationQueryCounter struct {
	sqlc.DBTX
	calls int
}

func (c *reservationQueryCounter) QueryContext(ctx context.Context,
	query string, args ...any) (*sql.Rows, error) {

	c.calls++
	return c.DBTX.QueryContext(ctx, query, args...)
}

func (c *reservationQueryCounter) QueryRowContext(ctx context.Context,
	query string, args ...any) *sql.Row {

	c.calls++
	return c.DBTX.QueryRowContext(ctx, query, args...)
}

func TestSqlStoreReservationSnapshot(t *testing.T) {
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	r := testReservation()
	require.NoError(t, store.CreateReservation(t.Context(), r))
	r.State = Ready
	require.NoError(t, store.UpdateReservation(t.Context(), r))
	queries := &reservationQueryCounter{DBTX: db.DB}
	readDB := *db.BaseDB
	readDB.Queries = sqlc.New(queries)
	got, err := NewSqlStore(&readDB).GetReservation(t.Context(), r.ID)
	require.NoError(t, err)
	require.Equal(t, r, got)
	require.Equal(t, 1, queries.calls)

	_, err = NewSqlStore(&readDB).GetReservation(t.Context(), ID{99})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.ExecContext(t.Context(),
		"DELETE FROM asset_reservation_updates WHERE reservation_id = $1", r.ID[:])
	require.NoError(t, err)
	_, err = NewSqlStore(&readDB).GetReservation(t.Context(), r.ID)
	require.ErrorContains(t, err, "missing reservation state history")
}

func TestSqlStoreStateFilter(t *testing.T) {
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	ctx := t.Context()
	saved := make(map[ID]*Reservation)
	for i, state := range []fsm.StateType{
		Ready, AwaitApproval, Canceled, Expired, NeedAdminAttention,
		QuoteRejected,
	} {
		r := testReservation()
		r.ID = ID{byte(i + 1)}
		require.NoError(t, store.CreateReservation(ctx, r))
		r.State = state
		require.NoError(t, store.UpdateReservation(ctx, r))
		saved[r.ID] = r
	}
	// All requested facts, including latest timestamps, must come from a
	// single statement on either database backend.
	queries := &reservationQueryCounter{DBTX: db.DB}
	readDB := *db.BaseDB
	readDB.Queries = sqlc.New(queries)
	store = NewSqlStore(&readDB)
	for _, tc := range []struct {
		name   string
		filter StateFilter
		ids    []ID
	}{
		{"all", StateFilter{}, []ID{{1}, {2}, {3}, {4}, {5}, {6}}},
		{"active", StateFilter{ActiveOnly: true}, []ID{{1}, {2}, {5}}},
		{"latest state", StateFilter{State: AwaitApproval}, []ID{{2}}},
		{"ready", StateFilter{State: Ready}, []ID{{1}}},
		{"rejected", StateFilter{State: QuoteRejected}, []ID{{6}}},
		{"admin", StateFilter{State: NeedAdminAttention}, []ID{{5}}},
		{"no matches", StateFilter{State: ProbeRoutes}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries.calls = 0
			rows, err := store.GetReservations(ctx, tc.filter)
			require.NoError(t, err)
			require.Equal(t, 1, queries.calls)
			var ids []ID
			for _, r := range rows {
				require.Equal(t, saved[r.ID], r)
				ids = append(ids, r.ID)
			}
			require.Equal(t, tc.ids, ids)
		})
	}
	_, err := store.GetReservations(ctx, StateFilter{
		State: "ready",
	})
	require.ErrorContains(t, err, "unknown reservation state")
	_, err = store.GetReservations(ctx, StateFilter{
		State: Ready, ActiveOnly: true,
	})
	require.ErrorContains(t, err, "mutually exclusive")

	// Excluded rows must not be decoded or validated. An invalid saved
	// quote on a completed purchase must not block recovery of active ones.
	completedID := ID{3}
	_, err = db.ExecContext(ctx, `UPDATE asset_reservations SET quote = $1
		WHERE reservation_id = $2`, []byte{0xff}, completedID[:])
	require.NoError(t, err)
	rows, err := store.GetReservations(ctx, StateFilter{ActiveOnly: true})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	_, err = store.GetReservations(ctx, StateFilter{})
	require.Error(t, err)
	activeID := ID{2}
	_, err = db.ExecContext(ctx, `DELETE FROM asset_reservation_updates
		WHERE reservation_id = $1`, activeID[:])
	require.NoError(t, err)
	_, err = store.GetReservations(ctx, StateFilter{ActiveOnly: true})
	require.Error(t, err)
}

func testReservation() *Reservation {
	_, pubkey := btcec.PrivKeyFromBytes([]byte{1})
	return &Reservation{
		ID: ID{1},
		Terms: Terms{
			AssetID:               [32]byte{2},
			Amount:                10000,
			Fee:                   11,
			CSVDelay:              1440,
			RequiredConfirmations: 3,
			ExecutionDelta:        90,
			MinUsableBlocks:       1000,
		},
		ClientKey: keychain.KeyDescriptor{
			PubKey: pubkey,
			KeyLocator: keychain.KeyLocator{
				Family: 99,
				Index:  math.MaxUint32,
			},
		},
		State: "AwaitApproval",
	}
}

func TestSqlStore(t *testing.T) {
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	now := time.Unix(1700000000, 0).UTC()
	store.clock = clock.NewTestClock(now)
	ctx := t.Context()
	r := testReservation()
	require.NoError(t, store.CreateReservation(ctx, r))
	require.Equal(t, now, r.CreatedAt)
	require.Equal(t, now, r.UpdatedAt)

	loaded, err := store.GetReservation(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, loaded)

	// Recovery returns the latest state and the original accepted fee.
	r.State = "Canceled"
	require.NoError(t, store.UpdateReservation(ctx, r))
	restarted := NewSqlStore(db.BaseDB)
	loaded, err = restarted.GetReservation(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, loaded)
	require.EqualValues(t, 11, loaded.Fee)

	// A repeated create restores progress instead of resetting it.
	retry := testReservation()
	require.NoError(t, restarted.CreateReservation(ctx, retry))
	require.Equal(t, r, retry)
	updates, err := db.GetAssetReservationUpdates(ctx, r.ID[:])
	require.NoError(t, err)
	require.Len(t, updates, 2)

	changed := *r
	changed.Fee++
	require.ErrorIs(t, store.CreateReservation(ctx, &changed),
		ErrRequestConflict)
	require.ErrorIs(t, store.UpdateReservation(ctx, &changed),
		ErrRequestConflict)
	changed = *r
	changed.ClientKey.Index--
	require.ErrorIs(t, store.CreateReservation(ctx, &changed),
		ErrRequestConflict)

	// Cancellation of a database call cannot advance persisted progress.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	changed = *r
	changed.State = "WaitForDelivery"
	store.clock = clock.NewTestClock(now.Add(time.Hour))
	require.Error(t, store.UpdateReservation(canceled, &changed))
	require.Equal(t, r.UpdatedAt, changed.UpdatedAt)
	loaded, err = store.GetReservation(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, loaded)

	all, err := store.GetReservations(ctx, StateFilter{})
	require.NoError(t, err)
	require.Equal(t, []*Reservation{r}, all)
	_, err = store.GetReservation(ctx, ID{99})
	require.ErrorIs(t, err, ErrNotFound)
	missing := *r
	missing.ID = ID{99}
	require.ErrorIs(t, store.UpdateReservation(ctx, &missing), ErrNotFound)

	// Damaged records fail closed instead of becoming recoverable purchases.
	row, err := db.GetAssetReservation(ctx, r.ID[:])
	require.NoError(t, err)
	_, err = toReservation(row, "", time.Time{})
	require.Error(t, err)
	row.ClientPubkey = make([]byte, 33)
	_, err = toReservation(row, updates[1].UpdateState, updates[1].UpdateTimestamp)
	require.Error(t, err)
}

func TestSqlStoreConcurrentCreate(t *testing.T) {
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	ctx := t.Context()
	results := make(chan *Reservation, 4)
	errors := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			r := testReservation()
			errors <- store.CreateReservation(ctx, r)
			results <- r
		})
	}
	wg.Wait()
	for range 4 {
		require.NoError(t, <-errors)
	}
	first := <-results
	for range 3 {
		require.Equal(t, first, <-results)
	}
	updates, err := db.GetAssetReservationUpdates(ctx, first.ID[:])
	require.NoError(t, err)
	require.Len(t, updates, 1)
}

func TestReservationValidation(t *testing.T) {
	id, err := NewID()
	require.NoError(t, err)
	require.NotEqual(t, ID{}, id)
	r := testReservation()
	require.NoError(t, r.Validate())
	r.ID = ID{}
	require.Error(t, r.Validate())
	r = testReservation()
	r.ClientKey.PubKey = nil
	require.Error(t, r.Validate())
	r = testReservation()
	r.ClientKey.Family = math.MaxUint32
	require.Error(t, r.Validate())
}
