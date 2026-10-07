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

	t := c.t

	depositOutpoints := make([]string, 0, len(deposits))
	for _, d := range deposits {
		depositOutpoints = append(depositOutpoints, d.OutPoint.String())
	}

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
