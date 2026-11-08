package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/stretchr/testify/require"
)

// newStoredDeposit creates a deposit in the store and returns it.
func newStoredDeposit(t *testing.T, store *SqlStore, seed byte) *Deposit {
	t.Helper()

	id, err := GetRandomDepositID()
	require.NoError(t, err)

	d := &Deposit{
		ID: id,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{seed},
			Index: uint32(seed),
		},
		Value:                100_000,
		TimeOutSweepPkScript: []byte{0x00, 0x14, seed},
	}
	d.SetState(Deposited)
	require.NoError(t, store.CreateDeposit(t.Context(), d))

	return d
}

// TestSqlStoreOnchainCost tests storing and loading the on-chain cost of
// deposits.
func TestSqlStoreOnchainCost(t *testing.T) {
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	store := NewSqlStore(testDb.BaseDB)
	ctx := t.Context()

	known := newStoredDeposit(t, store, 1)
	unknown := newStoredDeposit(t, store, 2)
	pending := newStoredDeposit(t, store, 3)

	// New deposits have no cost yet.
	for _, d := range []*Deposit{known, unknown, pending} {
		loaded, err := store.GetDeposit(ctx, d.ID)
		require.NoError(t, err)
		require.Equal(t, OnchainCost{}, loaded.OnchainCost)
	}

	knownCost := OnchainCost{Amount: 1_550, Status: OnchainCostKnown}
	unknownCost := OnchainCost{Status: OnchainCostUnknown}
	err := store.SetOnchainCosts(ctx, map[ID]OnchainCost{
		known.ID:   knownCost,
		unknown.ID: unknownCost,
	})
	require.NoError(t, err)

	loaded, err := store.GetDeposit(ctx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, OnchainCost{}, loaded.OnchainCost)

	loaded, err = store.GetDeposit(ctx, known.ID)
	require.NoError(t, err)
	require.Equal(t, knownCost, loaded.OnchainCost)

	loaded, err = store.DepositForOutpoint(ctx, unknown.OutPoint.String())
	require.NoError(t, err)
	require.Equal(t, unknownCost, loaded.OnchainCost)

	// A determined cost is set once: a later, different cost doesn't
	// overwrite it.
	err = store.SetOnchainCosts(ctx, map[ID]OnchainCost{
		known.ID:   unknownCost,
		unknown.ID: {Amount: 700, Status: OnchainCostKnown},
	})
	require.NoError(t, err)

	all, err := store.AllDeposits(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	byID := make(map[ID]*Deposit, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	require.Equal(t, knownCost, byID[known.ID].OnchainCost)
	require.Equal(t, unknownCost, byID[unknown.ID].OnchainCost)
	require.Equal(t, OnchainCost{}, byID[pending.ID].OnchainCost)

	// Updating a deposit keeps its cost.
	known.SetState(LoopingIn)
	require.NoError(t, store.UpdateDeposit(ctx, known))
	loaded, err = store.GetDeposit(ctx, known.ID)
	require.NoError(t, err)
	require.Equal(t, knownCost, loaded.OnchainCost)

	// An undetermined cost can't be stored, and nothing is stored then.
	err = store.SetOnchainCosts(ctx, map[ID]OnchainCost{
		pending.ID: {},
	})
	require.ErrorContains(t, err, "invalid on-chain cost status")
	loaded, err = store.GetDeposit(ctx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, OnchainCost{}, loaded.OnchainCost)

	// Nothing to store is fine.
	require.NoError(t, store.SetOnchainCosts(ctx, nil))
}

// TestSqlStoreCreateDepositOnchainCost tests that a deposit created with an
// on-chain cost is stored with that cost, and that a deposit without one is
// created with an undetermined cost.
func TestSqlStoreCreateDepositOnchainCost(t *testing.T) {
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	store := NewSqlStore(testDb.BaseDB)
	ctx := t.Context()

	knownCost := OnchainCost{Amount: 1_550, Status: OnchainCostKnown}
	unknownCost := OnchainCost{Status: OnchainCostUnknown}

	newDeposit := func(seed byte, cost OnchainCost) *Deposit {
		id, err := GetRandomDepositID()
		require.NoError(t, err)

		d := &Deposit{
			ID: id,
			OutPoint: wire.OutPoint{
				Hash: chainhash.Hash{seed},
			},
			Value:                100_000,
			TimeOutSweepPkScript: []byte{0x00, 0x14, seed},
			OnchainCost:          cost,
		}
		d.SetState(Deposited)
		require.NoError(t, store.CreateDeposit(ctx, d))

		return d
	}

	known := newDeposit(1, knownCost)
	unknown := newDeposit(2, unknownCost)
	pending := newDeposit(3, OnchainCost{})

	loaded, err := store.GetDeposit(ctx, known.ID)
	require.NoError(t, err)
	require.Equal(t, knownCost, loaded.OnchainCost)

	loaded, err = store.GetDeposit(ctx, unknown.ID)
	require.NoError(t, err)
	require.Equal(t, unknownCost, loaded.OnchainCost)

	loaded, err = store.GetDeposit(ctx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, OnchainCost{}, loaded.OnchainCost)

	// A deposit created with a cost keeps it.
	require.NoError(t, store.SetOnchainCosts(ctx, map[ID]OnchainCost{
		known.ID: unknownCost,
	}))
	loaded, err = store.GetDeposit(ctx, known.ID)
	require.NoError(t, err)
	require.Equal(t, knownCost, loaded.OnchainCost)
}
