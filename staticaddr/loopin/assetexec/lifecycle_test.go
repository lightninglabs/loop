package assetexec

import (
	"encoding/json"
	"testing"

	depositflow "github.com/lightninglabs/loop/staticaddr/deposit/flow"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

func depositState(t *testing.T, f *fixture, want depositflow.State) {
	t.Helper()
	r, err := f.store.Get(t.Context(), f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, want, r.DepositState)
}

func TestSharedDepositLifecycleRefund(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	depositState(t, f, depositflow.LoopingIn)
	f.a.pushErr = errUnavailable
	require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash), errUnavailable)
	depositState(t, f, depositflow.AssetClaimExposed)
	f.reopen()
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrPending)
	depositState(t, f, depositflow.SweepHtlcTimeout)
	require.Zero(t, f.a.releases)
	f.reopen()
	f.a.refund = []byte("confirmed-refund-proof")
	require.NoError(t, f.c.Cancel(ctx, f.offer.Hash))
	depositState(t, f, depositflow.HtlcTimeoutSwept)
	require.Equal(t, 1, f.a.releases)
}

func TestSharedDepositLifecycleClaimWins(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrPending)
	depositState(t, f, depositflow.SweepHtlcTimeout)
	f.a.preimageFound, f.a.settled = true, true
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	f.a.claim = []byte("confirmed-server-claim-proof")
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	depositState(t, f, depositflow.LoopedIn)
	require.Zero(t, f.a.cancels)
}

func TestSharedDepositLifecycleUnexposedCancel(t *testing.T) {
	f := newFixture(t)
	f.ready()
	require.NoError(t, f.c.Cancel(t.Context(), f.offer.Hash))
	depositState(t, f, depositflow.Deposited)
	require.Equal(t, 1, f.a.releases)
}

func TestOldJournalRecoversProtectedDepositState(t *testing.T) {
	f := newFixture(t)
	f.ready()
	require.NoError(t, f.c.ExposeClaim(t.Context(), f.offer.Hash))
	// Remove only the new field to emulate a journal from the previous
	// version; persisted exposure evidence must still retain ownership.
	err := f.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(recordBucket)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(bucket.Get(f.offer.Hash[:]),
			&fields); err != nil {

			return err
		}
		delete(fields, "DepositState")
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		return bucket.Put(f.offer.Hash[:], raw)
	})
	require.NoError(t, err)
	f.reopen()
	depositState(t, f, depositflow.AssetClaimExposed)
	r, err := f.store.Get(t.Context(), f.offer.Hash)
	require.NoError(t, err)
	r.Revision++
	r.DepositState = depositflow.Deposited
	require.ErrorIs(t, f.store.Update(
		t.Context(), r, r.Revision-1,
	), ErrInvalid)
}
