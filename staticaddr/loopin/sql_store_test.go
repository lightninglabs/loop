package loopin

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestGetStaticAddressLoopInSwapsByStates tests that we can retrieve
// StaticAddressLoopIn swaps by their states and that the deposits
// associated with the swaps are correctly populated.
func TestGetStaticAddressLoopInSwapsByStates(t *testing.T) {
	// Set up test context objects.
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	newID := func() deposit.ID {
		did, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		return did
	}

	loopingDepositID := newID()
	timeoutDepositID := newID()
	loopedInDepositID := newID()
	failedDepositID := newID()
	d1, d2, d3, d4 := &deposit.Deposit{
		ID: loopingDepositID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x1a, 0x2b, 0x3c, 0x4d},
			Index: 0,
		},
		Value: btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x41,
		},
	},
		&deposit.Deposit{
			ID: timeoutDepositID,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x2a, 0x2b, 0x3c, 0x4e},
				Index: 1,
			},
			Value: btcutil.Amount(200_000),
			TimeOutSweepPkScript: []byte{
				0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4d,
			},
		},
		&deposit.Deposit{
			ID: loopedInDepositID,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x3a, 0x2b, 0x3c, 0x4e},
				Index: 2,
			},
			Value: btcutil.Amount(300_000),
			TimeOutSweepPkScript: []byte{
				0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4f,
			},
		},
		&deposit.Deposit{
			ID: failedDepositID,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x4a, 0x2b, 0x3c, 0x4e},
				Index: 3,
			},
			Value: btcutil.Amount(400_000),
			TimeOutSweepPkScript: []byte{
				0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x50,
			},
		}

	err := depositStore.CreateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d2)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d3)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d4)
	require.NoError(t, err)

	// Add two updates per deposit, expect the last to be retrieved.
	d1.SetState(deposit.Deposited)
	d2.SetState(deposit.Deposited)
	d3.SetState(deposit.Deposited)
	d4.SetState(deposit.Deposited)

	err = depositStore.UpdateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d2)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d3)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d4)
	require.NoError(t, err)

	d1.SetState(deposit.LoopingIn)
	d2.SetState(deposit.HtlcTimeoutSwept)
	d3.SetState(deposit.LoopedIn)
	d4.SetState(deposit.Deposited)

	err = depositStore.UpdateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d2)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d3)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d4)
	require.NoError(t, err)

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	// Create pending swap.
	swapHashPending := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	swapPending := StaticAddressLoopIn{
		SwapHash:                swapHashPending,
		SwapPreimage:            lntypes.Preimage{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints:        []string{d1.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d1},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swapPending.SetState(SignHtlcTx)

	err = swapStore.CreateLoopIn(ctxb, &swapPending)
	require.NoError(t, err)

	// Create htlc-timeout-swept swap. HtlcTimeoutSwept is the first final
	// state, so this exercises the state-list query boundary.
	swapHashTimeoutSwept := lntypes.Hash{0x4, 0x2, 0x3, 0x5}
	swapTimeoutSwept := StaticAddressLoopIn{
		SwapHash:                swapHashTimeoutSwept,
		SwapPreimage:            lntypes.Preimage{0x4, 0x2, 0x3, 0x5},
		DepositOutpoints:        []string{d2.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d2},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swapTimeoutSwept.SetState(HtlcTimeoutSwept)

	err = swapStore.CreateLoopIn(ctxb, &swapTimeoutSwept)
	require.NoError(t, err)

	// Create succeeded swap.
	swapHashSucceeded := lntypes.Hash{0x2, 0x2, 0x3, 0x5}
	swapSucceeded := StaticAddressLoopIn{
		SwapHash:                swapHashSucceeded,
		SwapPreimage:            lntypes.Preimage{0x2, 0x2, 0x3, 0x5},
		DepositOutpoints:        []string{d3.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d3},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swapSucceeded.SetState(Succeeded)

	err = swapStore.CreateLoopIn(ctxb, &swapSucceeded)
	require.NoError(t, err)

	// Create failed swap. Failed is the last final state, so this
	// exercises the state-list query boundary.
	swapHashFailed := lntypes.Hash{0x3, 0x2, 0x3, 0x5}
	swapFailed := StaticAddressLoopIn{
		SwapHash:                swapHashFailed,
		SwapPreimage:            lntypes.Preimage{0x3, 0x2, 0x3, 0x5},
		DepositOutpoints:        []string{d4.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d4},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swapFailed.SetState(Failed)

	err = swapStore.CreateLoopIn(ctxb, &swapFailed)
	require.NoError(t, err)

	pendingSwaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(ctxb, PendingStates)
	require.NoError(t, err)

	require.Len(t, pendingSwaps, 1)
	require.Equal(t, swapHashPending, pendingSwaps[0].SwapHash)
	require.Equal(t, []string{d1.OutPoint.String()}, pendingSwaps[0].DepositOutpoints)
	require.Equal(t, SignHtlcTx, pendingSwaps[0].GetState())

	pendingDeposits := pendingSwaps[0].Deposits
	require.Len(t, pendingDeposits, 1)
	require.Equal(t, d1.ID, pendingDeposits[0].ID)
	require.Equal(t, d1.OutPoint, pendingDeposits[0].OutPoint)
	require.Equal(t, d1.Value, pendingDeposits[0].Value)
	require.Equal(t, deposit.LoopingIn, pendingDeposits[0].GetState())

	finalizedSwaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(ctxb, FinalStates)
	require.NoError(t, err)

	require.Len(t, finalizedSwaps, 3)
	finalizedByState := make(map[string]*StaticAddressLoopIn)
	for _, swap := range finalizedSwaps {
		finalizedByState[string(swap.GetState())] = swap
	}

	timeoutSweptSwap := finalizedByState[string(HtlcTimeoutSwept)]
	require.NotNil(t, timeoutSweptSwap)
	require.Equal(t, swapHashTimeoutSwept, timeoutSweptSwap.SwapHash)
	require.Equal(t, HtlcTimeoutSwept, timeoutSweptSwap.GetState())

	succeededSwap := finalizedByState[string(Succeeded)]
	require.NotNil(t, succeededSwap)
	require.Equal(t, swapHashSucceeded, succeededSwap.SwapHash)
	require.Equal(t, []string{d3.OutPoint.String()}, succeededSwap.DepositOutpoints)
	require.Equal(t, Succeeded, succeededSwap.GetState())

	failedSwap := finalizedByState[string(Failed)]
	require.NotNil(t, failedSwap)
	require.Equal(t, swapHashFailed, failedSwap.SwapHash)
	require.Equal(t, Failed, failedSwap.GetState())

	finalizedDeposits := succeededSwap.Deposits
	require.Len(t, finalizedDeposits, 1)
	require.Equal(t, d3.ID, finalizedDeposits[0].ID)
	require.Equal(t, d3.OutPoint, finalizedDeposits[0].OutPoint)
	require.Equal(t, d3.Value, finalizedDeposits[0].Value)
	require.Equal(t, deposit.LoopedIn, finalizedDeposits[0].GetState())
}

// TestCreateLoopIn tests that CreateLoopIn correctly creates a new
// StaticAddressLoopIn swap and associates it with the provided deposits.
func TestCreateLoopIn(t *testing.T) {
	// Set up test context objects.
	ctx := t.Context()
	testDb := loopdb.NewTestDB(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	newID := func() deposit.ID {
		did, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		return did
	}

	d1, d2 := &deposit.Deposit{
		ID: newID(),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x1a, 0x2b, 0x3c, 0x4d},
			Index: 0,
		},
		Value: btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x41,
		},
	},
		&deposit.Deposit{
			ID: newID(),
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x2a, 0x2b, 0x3c, 0x4e},
				Index: 1,
			},
			Value: btcutil.Amount(200_000),
			TimeOutSweepPkScript: []byte{
				0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4d,
			},
		}

	err := depositStore.CreateDeposit(ctx, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctx, d2)
	require.NoError(t, err)

	d1.SetState(deposit.LoopingIn)
	d2.SetState(deposit.LoopingIn)

	err = depositStore.UpdateDeposit(ctx, d1)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctx, d2)
	require.NoError(t, err)

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	// Create pending swap.
	swapHashPending := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	swapPending := StaticAddressLoopIn{
		SwapHash:     swapHashPending,
		SwapPreimage: lntypes.Preimage{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints: []string{d1.OutPoint.String(),
			d2.OutPoint.String()},
		Deposits:     []*deposit.Deposit{d1, d2},
		ClientPubkey: clientPubKey,
		ServerPubkey: serverPubKey,
		HtlcKeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  37,
		},
		HtlcTimeoutSweepAddress: addr,
	}
	swapPending.SetState(SignHtlcTx)

	err = swapStore.CreateLoopIn(ctx, &swapPending)
	require.NoError(t, err)

	depositIDs, err := swapStore.DepositIDsForSwapHash(
		ctx, swapHashPending,
	)
	require.NoError(t, err)
	require.Len(t, depositIDs, 2)
	require.Contains(t, depositIDs, d1.ID)
	require.Contains(t, depositIDs, d2.ID)

	swapHashes, err := swapStore.SwapHashesForDepositIDs(
		ctx, []deposit.ID{depositIDs[0], depositIDs[1]},
	)
	require.NoError(t, err)
	require.Len(t, swapHashes, 1)
	require.Len(t, swapHashes[swapHashPending], 2)
	require.Contains(t, swapHashes[swapHashPending], depositIDs[0])
	require.Contains(t, swapHashes[swapHashPending], depositIDs[1])

	swap, err := swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(t, swapHashPending, swap.SwapHash)
	require.Equal(t, []string{d1.OutPoint.String(), d2.OutPoint.String()},
		swap.DepositOutpoints)
	require.Equal(t, SignHtlcTx, swap.GetState())
	require.Equal(t, swapPending.HtlcKeyLocator, swap.HtlcKeyLocator)
	require.Equal(
		t, ConfirmationRiskDecisionNone,
		swap.ConfirmationRiskDecision,
	)

	decisionTime := time.Unix(123, 0).UTC()
	testClock.SetTime(decisionTime)
	err = swapStore.RecordStaticAddressRiskDecision(
		ctx, swapHashPending, ConfirmationRiskDecisionAccepted,
	)
	require.NoError(t, err)

	swap, err = swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(
		t, ConfirmationRiskDecisionAccepted,
		swap.ConfirmationRiskDecision,
	)
	require.True(t, swap.ConfirmationRiskDecisionTime.Equal(decisionTime))

	err = swapStore.RecordStaticAddressRiskDecision(
		ctx, lntypes.Hash{0x9, 0x9, 0x9},
		ConfirmationRiskDecisionRejected,
	)
	require.ErrorIs(t, err, ErrLoopInNotFound)

	require.Len(t, swap.Deposits, 2)

	require.Equal(t, d1.ID, swap.Deposits[0].ID)
	require.Equal(t, d1.OutPoint, swap.Deposits[0].OutPoint)
	require.Equal(t, d1.Value, swap.Deposits[0].Value)
	require.Equal(t, deposit.LoopingIn, swap.Deposits[0].GetState())

	require.Equal(t, d2.ID, swap.Deposits[1].ID)
	require.Equal(t, d2.OutPoint, swap.Deposits[1].OutPoint)
	require.Equal(t, d2.Value, swap.Deposits[1].Value)
	require.Equal(t, deposit.LoopingIn, swap.Deposits[1].GetState())

	updateTime := testClock.Now().Add(time.Minute)
	testClock.SetTime(updateTime)
	swapPending.SetState(Succeeded)

	err = swapStore.UpdateLoopIn(ctx, &swapPending)
	require.NoError(t, err)

	swap, err = swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(t, Succeeded, swap.GetState())
	require.WithinDuration(
		t, updateTime.UTC(), swap.LastUpdateTime.UTC(),
		time.Microsecond,
	)
}

// TestGetLoopInByHashOrdersDepositsBySnapshot ensures recovered deposits are
// ordered by the stored swap input snapshot, which is the signing order shared
// with the server.
func TestGetLoopInByHashOrdersDepositsBySnapshot(t *testing.T) {
	ctx := context.Background()
	testDb := loopdb.NewTestDB(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	newID := func() deposit.ID {
		did, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		return did
	}

	d1 := &deposit.Deposit{
		ID: newID(),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		},
		Value: 100_000,
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x41,
		},
	}
	d2 := &deposit.Deposit{
		ID: newID(),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x22},
			Index: 1,
		},
		Value: 200_000,
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4d,
		},
	}

	require.NoError(t, depositStore.CreateDeposit(ctx, d1))
	require.NoError(t, depositStore.CreateDeposit(ctx, d2))

	d1.SetState(deposit.LoopingIn)
	d2.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.UpdateDeposit(ctx, d1))
	require.NoError(t, depositStore.UpdateDeposit(ctx, d2))

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	swap := StaticAddressLoopIn{
		SwapHash:     swapHash,
		SwapPreimage: lntypes.Preimage{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints: []string{
			d2.OutPoint.String(), d1.OutPoint.String(),
		},
		Deposits:                []*deposit.Deposit{d2, d1},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swap.SetState(SignHtlcTx)

	require.NoError(t, swapStore.CreateLoopIn(ctx, &swap))

	storedSwap, err := swapStore.GetLoopInByHash(ctx, swapHash)
	require.NoError(t, err)
	require.Equal(t, []string{
		d2.OutPoint.String(), d1.OutPoint.String(),
	}, storedSwap.DepositOutpoints)
	require.Len(t, storedSwap.Deposits, 2)
	require.Equal(t, d2.ID, storedSwap.Deposits[0].ID)
	require.Equal(t, d1.ID, storedSwap.Deposits[1].ID)
}

func TestUpdateLoopInPersistsConfirmedHtlcOutpoint(t *testing.T) {
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	depositID, err := deposit.GetRandomDepositID()
	require.NoError(t, err)

	d := &deposit.Deposit{
		ID: depositID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x1a, 0x2b, 0x3c, 0x4d},
			Index: 0,
		},
		Value: btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x41,
		},
	}
	require.NoError(t, depositStore.CreateDeposit(ctxb, d))

	d.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.UpdateDeposit(ctxb, d))

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x4, 0x2, 0x3, 0x5}
	swap := StaticAddressLoopIn{
		SwapHash:                swapHash,
		SwapPreimage:            lntypes.Preimage{0x4, 0x2, 0x3, 0x5},
		DepositOutpoints:        []string{d.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swap.SetState(MonitorInvoiceAndHtlcTx)
	require.NoError(t, swapStore.CreateLoopIn(ctxb, &swap))

	confirmedHtlcTxHash := chainhash.Hash{0x55}
	swap.HtlcTxHash = &confirmedHtlcTxHash
	swap.HtlcOutputIndex = 2
	swap.HtlcOutputValue = 88_000
	require.NoError(t, swapStore.UpdateLoopIn(ctxb, &swap))

	storedSwap, err := swapStore.GetLoopInByHash(ctxb, swapHash)
	require.NoError(t, err)
	require.NotNil(t, storedSwap.HtlcTxHash)
	require.Equal(t, confirmedHtlcTxHash, *storedSwap.HtlcTxHash)
	require.EqualValues(t, 2, storedSwap.HtlcOutputIndex)
	require.EqualValues(t, 88_000, storedSwap.HtlcOutputValue)
	require.Equal(t, MonitorInvoiceAndHtlcTx, storedSwap.GetState())
}

// TestGetLoopInByHashPreservesStoredDepositOutpoints ensures recovered loop-ins
// keep the original outpoint snapshot stored when the swap was created.
func TestGetLoopInByHashPreservesStoredDepositOutpoints(t *testing.T) {
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	depositID, err := deposit.GetRandomDepositID()
	require.NoError(t, err)

	oldOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x1a, 0x2b, 0x3c, 0x4d},
		Index: 0,
	}
	currentOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x5a, 0x6b, 0x7c, 0x8d},
		Index: 1,
	}

	d := &deposit.Deposit{
		ID:       depositID,
		OutPoint: oldOutpoint,
		Value:    btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x41,
		},
	}
	require.NoError(t, depositStore.CreateDeposit(ctxb, d))

	d.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.UpdateDeposit(ctxb, d))

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	swap := StaticAddressLoopIn{
		SwapHash:                swapHash,
		SwapPreimage:            lntypes.Preimage{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints:        []string{oldOutpoint.String()},
		Deposits:                []*deposit.Deposit{d},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	swap.SetState(SignHtlcTx)

	require.NoError(t, swapStore.CreateLoopIn(ctxb, &swap))

	d.OutPoint = currentOutpoint
	d.ConfirmationHeight = 42
	require.NoError(t, depositStore.UpdateDeposit(ctxb, d))

	storedSwap, err := swapStore.GetLoopInByHash(ctxb, swapHash)
	require.NoError(t, err)
	require.Equal(
		t, []string{oldOutpoint.String()},
		storedSwap.DepositOutpoints,
	)
	require.Len(t, storedSwap.Deposits, 1)
	require.Equal(t, currentOutpoint, storedSwap.Deposits[0].OutPoint)
	require.Equal(t, int64(42), storedSwap.Deposits[0].ConfirmationHeight)
}
