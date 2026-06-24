package loopin

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
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
		Deposits:                []*deposit.Deposit{d1, d2},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
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

// TestGetLoopInByHashRejectsTruncatedTimeoutSweepTxID verifies that a
// persisted short timeout sweep txid is rejected during swap loading.
func TestGetLoopInByHashRejectsTruncatedTimeoutSweepTxID(t *testing.T) {
	ctxb := t.Context()
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

	depositRecord := &deposit.Deposit{
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

	err = depositStore.CreateDeposit(ctxb, depositRecord)
	require.NoError(t, err)

	depositRecord.SetState(deposit.LoopingIn)
	err = depositStore.UpdateDeposit(ctxb, depositRecord)
	require.NoError(t, err)

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	loopIn := StaticAddressLoopIn{
		SwapHash:     swapHash,
		SwapPreimage: lntypes.Preimage{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints: []string{
			depositRecord.OutPoint.String(),
		},
		Deposits:                []*deposit.Deposit{depositRecord},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	loopIn.SetState(SignHtlcTx)

	err = swapStore.CreateLoopIn(ctxb, &loopIn)
	require.NoError(t, err)

	err = testDb.Queries.UpdateStaticAddressLoopIn(
		ctxb, sqlc.UpdateStaticAddressLoopInParams{
			SwapHash: swapHash[:],
			HtlcTimeoutSweepTxID: sql.NullString{
				String: "abcd",
				Valid:  true,
			},
		},
	)
	require.NoError(t, err)

	_, err = swapStore.GetLoopInByHash(ctxb, swapHash)
	require.ErrorContains(t, err, "invalid htlc timeout sweep txid")
}

// TestGetLoopInByHashAllowsEmptyTimeoutSweepTxID verifies that empty persisted
// timeout sweep txids are tolerated for recovery robustness.
func TestGetLoopInByHashAllowsEmptyTimeoutSweepTxID(t *testing.T) {
	ctxb := t.Context()
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

	depositRecord := &deposit.Deposit{
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

	err = depositStore.CreateDeposit(ctxb, depositRecord)
	require.NoError(t, err)

	depositRecord.SetState(deposit.LoopingIn)
	err = depositStore.UpdateDeposit(ctxb, depositRecord)
	require.NoError(t, err)

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	loopIn := StaticAddressLoopIn{
		SwapHash:     swapHash,
		SwapPreimage: lntypes.Preimage{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints: []string{
			depositRecord.OutPoint.String(),
		},
		Deposits:                []*deposit.Deposit{depositRecord},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	loopIn.SetState(SignHtlcTx)

	err = swapStore.CreateLoopIn(ctxb, &loopIn)
	require.NoError(t, err)

	err = testDb.Queries.UpdateStaticAddressLoopIn(
		ctxb, sqlc.UpdateStaticAddressLoopInParams{
			SwapHash: swapHash[:],
			HtlcTimeoutSweepTxID: sql.NullString{
				String: "",
				Valid:  true,
			},
		},
	)
	require.NoError(t, err)

	swap, err := swapStore.GetLoopInByHash(ctxb, swapHash)
	require.NoError(t, err)
	require.Nil(t, swap.HtlcTimeoutSweepTxHash)
}
