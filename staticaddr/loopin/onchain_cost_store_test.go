package loopin

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// onchainCostTestContext holds the stores used by the on-chain cost tests.
type onchainCostTestContext struct {
	t            *testing.T
	db           *loopdb.StoreMock
	depositStore *deposit.SqlStore
	swapStore    *SqlStore
}

// newOnchainCostTestContext creates a test context with empty stores.
func newOnchainCostTestContext(t *testing.T) *onchainCostTestContext {
	testDb := loopdb.NewTestDB(t)
	t.Cleanup(func() { testDb.Close() })

	return &onchainCostTestContext{
		t:            t,
		db:           loopdb.NewStoreMock(t),
		depositStore: deposit.NewSqlStore(testDb.BaseDB),
		swapStore: NewSqlStore(
			loopdb.NewTypedStore[Querier](testDb),
			clock.NewTestClock(time.Now()), &chaincfg.MainNetParams,
		),
	}
}

// createLoopIn stores a loop-in in the given state that uses the given
// outputs of funding transactions as new deposits.
func (c *onchainCostTestContext) createLoopIn(swapHash lntypes.Hash,
	state fsm.StateType, outpoints ...wire.OutPoint) *StaticAddressLoopIn {

	t := c.t

	deposits := make([]*deposit.Deposit, 0, len(outpoints))
	for _, outpoint := range outpoints {
		id, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		d := &deposit.Deposit{
			ID:                   id,
			OutPoint:             outpoint,
			Value:                btcutil.Amount(100_000),
			ConfirmationHeight:   100,
			TimeOutSweepPkScript: []byte{0x00, 0x14, 0x1a},
		}
		require.NoError(t, c.depositStore.CreateDeposit(t.Context(), d))

		deposits = append(deposits, d)
	}

	return c.createLoopInWithDeposits(swapHash, state, deposits)
}

// createLoopInWithDeposits stores a loop-in in the given state that uses the
// given stored deposits.
func (c *onchainCostTestContext) createLoopInWithDeposits(
	swapHash lntypes.Hash, state fsm.StateType,
	deposits []*deposit.Deposit) *StaticAddressLoopIn {

	depositOutpoints := make([]string, 0, len(deposits))
	for _, d := range deposits {
		depositOutpoints = append(depositOutpoints, d.OutPoint.String())
	}

	return c.createLoopInWithOutpoints(
		swapHash, state, deposits, depositOutpoints,
	)
}

// createLoopInWithOutpoints stores a loop-in in the given state that uses the
// given stored deposits and records the given deposit outpoints.
func (c *onchainCostTestContext) createLoopInWithOutpoints(
	swapHash lntypes.Hash, state fsm.StateType, deposits []*deposit.Deposit,
	depositOutpoints []string) *StaticAddressLoopIn {

	t := c.t

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(
		"bcrt1qq68r6ff4k4pjx39efs44gcyccf7unqnu5qtjjz", nil,
	)
	require.NoError(t, err)

	swapPreimage := lntypes.Preimage(swapHash)
	loopIn := &StaticAddressLoopIn{
		SwapHash:                swapHash,
		SwapPreimage:            swapPreimage,
		DepositOutpoints:        depositOutpoints,
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
		Deposits:                deposits,
	}
	loopIn.SetState(state)

	require.NoError(t, c.swapStore.CreateLoopIn(t.Context(), loopIn))

	return loopIn
}

// onchainCost reloads a loop-in from the database and returns its on-chain
// cost.
func (c *onchainCostTestContext) onchainCost(
	swapHash lntypes.Hash) *btcutil.Amount {

	loopIn, err := c.swapStore.GetLoopInByHash(c.t.Context(), swapHash)
	require.NoError(c.t, err)

	return loopIn.OnchainCost
}

// TestSqlStoreOnchainCost tests that the on-chain cost survives a reload from
// the database, that an unknown cost stays unknown, and that a stored cost
// is never cleared.
func TestSqlStoreOnchainCost(t *testing.T) {
	c := newOnchainCostTestContext(t)
	loopIn := c.createLoopIn(
		lntypes.Hash{0x1}, MonitorInvoiceAndHtlcTx,
		wire.OutPoint{Hash: chainhash.Hash{0x1}, Index: 0},
	)
	require.Nil(t, c.onchainCost(loopIn.SwapHash))

	// Updating the swap without a cost keeps it unknown.
	loopIn.SetState(PaymentReceived)
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), loopIn))
	require.Nil(t, c.onchainCost(loopIn.SwapHash))

	// The cost is persisted together with the state.
	cost := btcutil.Amount(1_550)
	loopIn.OnchainCost = &cost
	loopIn.SetState(Succeeded)
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), loopIn))

	stored := c.onchainCost(loopIn.SwapHash)
	require.NotNil(t, stored)
	require.Equal(t, cost, *stored)

	// A swap reloaded from the database keeps its cost on later updates.
	reloaded, err := c.swapStore.GetLoopInByHash(
		t.Context(), loopIn.SwapHash,
	)
	require.NoError(t, err)
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), reloaded))
	require.Equal(t, cost, *c.onchainCost(loopIn.SwapHash))

	// An update with an unknown cost doesn't clear a stored cost.
	loopIn.OnchainCost = nil
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), loopIn))
	require.Equal(t, cost, *c.onchainCost(loopIn.SwapHash))
}

// TestBatchSetUnknownOnchainCosts tests that the batch update only sets the
// cost of swaps that don't have one yet.
func TestBatchSetUnknownOnchainCosts(t *testing.T) {
	c := newOnchainCostTestContext(t)
	unknown := c.createLoopIn(
		lntypes.Hash{0x1}, Succeeded,
		wire.OutPoint{Hash: chainhash.Hash{0x1}, Index: 0},
	)
	known := c.createLoopIn(
		lntypes.Hash{0x2}, Succeeded,
		wire.OutPoint{Hash: chainhash.Hash{0x2}, Index: 0},
	)
	storedCost := btcutil.Amount(42)
	known.OnchainCost = &storedCost
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), known))

	err := c.swapStore.BatchSetUnknownOnchainCosts(
		t.Context(), map[lntypes.Hash]btcutil.Amount{
			unknown.SwapHash: 700,
			known.SwapHash:   700,
		},
	)
	require.NoError(t, err)

	require.Equal(t, btcutil.Amount(700), *c.onchainCost(unknown.SwapHash))
	require.Equal(t, storedCost, *c.onchainCost(known.SwapHash))
}

// TestSwapsWithoutOnchainCost tests that only swaps without a cost are
// returned, with their latest state. Updates with the same timestamp are
// ordered by insertion.
func TestSwapsWithoutOnchainCost(t *testing.T) {
	c := newOnchainCostTestContext(t)

	// The test clock doesn't advance, so both updates of the swap have the
	// same timestamp.
	outpoint := wire.OutPoint{Hash: chainhash.Hash{0x1}, Index: 0}
	swap := c.createLoopIn(
		lntypes.Hash{0x1}, MonitorInvoiceAndHtlcTx, outpoint,
	)
	swap.SetState(Succeeded)
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), swap))

	known := c.createLoopIn(
		lntypes.Hash{0x2}, Succeeded,
		wire.OutPoint{Hash: chainhash.Hash{0x2}, Index: 0},
	)
	cost := btcutil.Amount(42)
	known.OnchainCost = &cost
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), known))

	swaps, err := c.swapStore.swapsWithoutOnchainCost(t.Context())
	require.NoError(t, err)
	require.Equal(t, []swapWithoutOnchainCost{{
		SwapHash:         swap.SwapHash,
		State:            Succeeded,
		DepositOutpoints: []string{outpoint.String()},
	}}, swaps)
}

// TestSwapsWithoutOnchainCostLatestUpdate tests that the latest state of a
// swap is taken from the update with the latest timestamp, not the latest
// inserted one.
func TestSwapsWithoutOnchainCostLatestUpdate(t *testing.T) {
	testDb := loopdb.NewTestDB(t)
	t.Cleanup(func() { testDb.Close() })

	now := time.Now()
	testClock := clock.NewTestClock(now)
	c := &onchainCostTestContext{
		t:            t,
		db:           loopdb.NewStoreMock(t),
		depositStore: deposit.NewSqlStore(testDb.BaseDB),
		swapStore: NewSqlStore(
			loopdb.NewTypedStore[Querier](testDb), testClock,
			&chaincfg.MainNetParams,
		),
	}

	swap := c.createLoopIn(
		lntypes.Hash{0x1}, MonitorInvoiceAndHtlcTx,
		wire.OutPoint{Hash: chainhash.Hash{0x1}, Index: 0},
	)

	// The latest update is inserted before an update with an earlier
	// timestamp.
	testClock.SetTime(now.Add(2 * time.Second))
	swap.SetState(Succeeded)
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), swap))

	testClock.SetTime(now.Add(time.Second))
	swap.SetState(PaymentReceived)
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), swap))

	swaps, err := c.swapStore.swapsWithoutOnchainCost(t.Context())
	require.NoError(t, err)
	require.Len(t, swaps, 1)
	require.Equal(t, Succeeded, swaps[0].State)
}

// TestToNullAmount tests that an unknown amount is stored as NULL, while a
// known amount, including zero, is stored as a value.
func TestToNullAmount(t *testing.T) {
	zero := btcutil.Amount(0)
	cost := btcutil.Amount(1_550)

	tests := []struct {
		name   string
		amount *btcutil.Amount
		want   sql.NullInt64
	}{
		{
			name:   "unknown",
			amount: nil,
			want:   sql.NullInt64{},
		},
		{
			name:   "known zero",
			amount: &zero,
			want:   sql.NullInt64{Int64: 0, Valid: true},
		},
		{
			name:   "known amount",
			amount: &cost,
			want:   sql.NullInt64{Int64: 1_550, Valid: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, toNullAmount(tc.amount))
		})
	}
}
