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
	loopedInDepositID := newID()
	failedDepositID := newID()
	d1, d2, d3 := &deposit.Deposit{
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
			ID: loopedInDepositID,
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
			ID: failedDepositID,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x3a, 0x2b, 0x3c, 0x4e},
				Index: 2,
			},
			Value: btcutil.Amount(300_000),
			TimeOutSweepPkScript: []byte{
				0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4f,
			},
		}

	err := depositStore.CreateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d2)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d3)
	require.NoError(t, err)

	// Add two updates per deposit, expect the last to be retrieved.
	d1.SetState(deposit.Deposited)
	d2.SetState(deposit.Deposited)
	d3.SetState(deposit.Deposited)

	err = depositStore.UpdateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d2)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d3)
	require.NoError(t, err)

	d1.SetState(deposit.LoopingIn)
	d2.SetState(deposit.LoopedIn)
	d3.SetState(deposit.Deposited)

	err = depositStore.UpdateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d2)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d3)
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

	// Create succeeded swap.
	swapHashSucceeded := lntypes.Hash{0x2, 0x2, 0x3, 0x5}
	swapSucceeded := StaticAddressLoopIn{
		SwapHash:                swapHashSucceeded,
		SwapPreimage:            lntypes.Preimage{0x2, 0x2, 0x3, 0x5},
		DepositOutpoints:        []string{d2.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d2},
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
		DepositOutpoints:        []string{d3.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d3},
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

	require.Len(t, finalizedSwaps, 2)
	require.Equal(t, swapHashSucceeded, finalizedSwaps[0].SwapHash)
	require.Equal(t, []string{d2.OutPoint.String()}, finalizedSwaps[0].DepositOutpoints)
	require.Equal(t, Succeeded, finalizedSwaps[0].GetState())
	require.Equal(t, swapHashFailed, finalizedSwaps[1].SwapHash)
	require.Equal(t, Failed, finalizedSwaps[1].GetState())

	finalizedDeposits := finalizedSwaps[0].Deposits
	require.Len(t, finalizedDeposits, 1)
	require.Equal(t, d2.ID, finalizedDeposits[0].ID)
	require.Equal(t, d2.OutPoint, finalizedDeposits[0].OutPoint)
	require.Equal(t, d2.Value, finalizedDeposits[0].Value)
	require.Equal(t, deposit.LoopedIn, finalizedDeposits[0].GetState())
}

// TestCreateLoopIn tests that CreateLoopIn correctly creates a new
// StaticAddressLoopIn swap and associates it with the provided deposits.
func TestCreateLoopIn(t *testing.T) {
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

	err := depositStore.CreateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d2)
	require.NoError(t, err)

	d1.SetState(deposit.LoopingIn)
	d2.SetState(deposit.LoopingIn)

	err = depositStore.UpdateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.UpdateDeposit(ctxb, d2)
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

	err = swapStore.CreateLoopIn(ctxb, &swapPending)
	require.NoError(t, err)

	depositIDs, err := swapStore.DepositIDsForSwapHash(
		ctxb, swapHashPending,
	)
	require.NoError(t, err)
	require.Len(t, depositIDs, 2)
	require.Contains(t, depositIDs, d1.ID)
	require.Contains(t, depositIDs, d2.ID)

	swapHashes, err := swapStore.SwapHashesForDepositIDs(
		ctxb, []deposit.ID{depositIDs[0], depositIDs[1]},
	)
	require.NoError(t, err)
	require.Len(t, swapHashes, 1)
	require.Len(t, swapHashes[swapHashPending], 2)
	require.Contains(t, swapHashes[swapHashPending], depositIDs[0])
	require.Contains(t, swapHashes[swapHashPending], depositIDs[1])

	storedSwap, err := swapStore.GetLoopInByHash(ctxb, swapHashPending)
	require.NoError(t, err)
	require.Equal(t, swapHashPending, storedSwap.SwapHash)
	require.Equal(t, []string{d1.OutPoint.String(), d2.OutPoint.String()},
		storedSwap.DepositOutpoints)
	require.Equal(t, SignHtlcTx, storedSwap.GetState())
	require.Equal(t, swapPending.HtlcKeyLocator, storedSwap.HtlcKeyLocator)
	require.Equal(
		t, ConfirmationRiskDecisionNone,
		storedSwap.ConfirmationRiskDecision,
	)

	decisionTime := time.Unix(123, 0).UTC()
	testClock.SetTime(decisionTime)
	err = swapStore.RecordStaticAddressRiskDecision(
		ctxb, swapHashPending, ConfirmationRiskDecisionAccepted,
	)
	require.NoError(t, err)

	storedSwap, err = swapStore.GetLoopInByHash(ctxb, swapHashPending)
	require.NoError(t, err)
	require.Equal(
		t, ConfirmationRiskDecisionAccepted,
		storedSwap.ConfirmationRiskDecision,
	)
	require.True(t, storedSwap.ConfirmationRiskDecisionTime.Equal(decisionTime))

	err = swapStore.RecordStaticAddressRiskDecision(
		ctxb, lntypes.Hash{0x9, 0x9, 0x9},
		ConfirmationRiskDecisionRejected,
	)
	require.ErrorIs(t, err, ErrLoopInNotFound)

	require.Len(t, storedSwap.Deposits, 2)

	require.Equal(t, d1.ID, storedSwap.Deposits[0].ID)
	require.Equal(t, d1.OutPoint, storedSwap.Deposits[0].OutPoint)
	require.Equal(t, d1.Value, storedSwap.Deposits[0].Value)
	require.Equal(t, deposit.LoopingIn, storedSwap.Deposits[0].GetState())

	require.Equal(t, d2.ID, storedSwap.Deposits[1].ID)
	require.Equal(t, d2.OutPoint, storedSwap.Deposits[1].OutPoint)
	require.Equal(t, d2.Value, storedSwap.Deposits[1].Value)
	require.Equal(t, deposit.LoopingIn, storedSwap.Deposits[1].GetState())
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
