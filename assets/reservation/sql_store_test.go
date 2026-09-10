package reservation

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

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

	all, err := store.GetReservations(ctx)
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
	_, err = toReservation(row, nil)
	require.Error(t, err)
	row.ClientPubkey = make([]byte, 33)
	_, err = toReservation(row, updates)
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
