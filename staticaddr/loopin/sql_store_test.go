package loopin

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestLoopInFundingTxSnapshot persists unique unconfirmed parents atomically,
// without witness data, and leaves absent legacy snapshots conservative.
func TestLoopInFundingTxSnapshot(t *testing.T) {
	for _, withSnapshot := range []bool{false, true} {
		name := "legacy without snapshot"
		if withSnapshot {
			name = "shared funding parent"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			db := loopdb.NewTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			store := NewSqlStore(
				loopdb.NewTypedStore[Querier](db),
				clock.NewTestClock(time.Now()),
				&chaincfg.RegressionNetParams,
			)
			depositStore := deposit.NewSqlStore(db.BaseDB)
			deposits := []*deposit.Deposit{
				{ID: deposit.ID{1}, Value: 100_000},
				{ID: deposit.ID{2}, Value: 150_000},
			}
			setPersistedTestDepositAddress(t, ctx, db.BaseDB, deposits...)
			parent := wire.NewMsgTx(2)
			parent.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}},
				Witness:          wire.TxWitness{{1, 2, 3}},
			})
			for i, d := range deposits {
				parent.AddTxOut(wire.NewTxOut(
					int64(d.Value), d.AddressParams.PkScript,
				))
				d.Index = uint32(i)
			}
			for _, d := range deposits {
				d.Hash = parent.TxHash()
				d.TimeOutSweepPkScript = []byte{0x51}
				d.SetState(deposit.LoopingIn)
				require.NoError(t, depositStore.CreateDeposit(ctx, d))
			}
			_, clientKey := test.CreateKey(1)
			_, serverKey := test.CreateKey(2)
			timeoutAddress, err := btcutil.DecodeAddress(P2wkhAddr, nil)
			require.NoError(t, err)
			swap := &StaticAddressLoopIn{
				SwapHash:     lntypes.Hash{1},
				SwapPreimage: lntypes.Preimage{2},
				Deposits:     deposits,
				DepositOutpoints: []string{
					deposits[0].OutPoint.String(),
					deposits[1].OutPoint.String(),
				},
				ClientPubkey:            clientKey,
				ServerPubkey:            serverKey,
				HtlcTimeoutSweepAddress: timeoutAddress,
			}
			if withSnapshot {
				// Both selected outputs share this parent. Witness changes
				// do not affect its txid or create a second stored parent.
				duplicate := parent.Copy()
				duplicate.TxIn[0].Witness = wire.TxWitness{{4, 5, 6}}
				swap.FundingTxs = []*wire.MsgTx{parent, duplicate}
			}
			swap.SetState(SignHtlcTx)
			if withSnapshot {
				parents := swap.FundingTxs
				swap.FundingTxs = []*wire.MsgTx{nil}
				require.Error(t, store.CreateLoopIn(ctx, swap))
				stored, err := store.IsStored(ctx, swap.SwapHash)
				require.NoError(t, err)
				require.False(t, stored)
				swap.FundingTxs = parents
			}
			require.NoError(t, store.CreateLoopIn(ctx, swap))

			var raw bytes.Buffer
			require.NoError(t, parent.SerializeNoWitness(&raw))
			assertSnapshot := func(loaded *StaticAddressLoopIn) {
				t.Helper()
				if !withSnapshot {
					require.Nil(t, loaded.FundingTxs)
					return
				}
				require.Len(t, loaded.FundingTxs, 1)
				require.Equal(t, parent.TxHash(), loaded.FundingTxs[0].TxHash())
				require.False(t, loaded.FundingTxs[0].HasWitness())
				var restored bytes.Buffer
				require.NoError(t, loaded.FundingTxs[0].SerializeNoWitness(&restored))
				require.Equal(t, raw.Bytes(), restored.Bytes())
			}
			assertBothReads := func() {
				t.Helper()
				loaded, err := store.GetLoopInByHash(ctx, swap.SwapHash)
				require.NoError(t, err)
				assertSnapshot(loaded)
				loadedSwaps, err := store.GetStaticAddressLoopInSwapsByStates(
					ctx, []fsm.StateType{swap.GetState()},
				)
				require.NoError(t, err)
				require.Len(t, loadedSwaps, 1)
				assertSnapshot(loadedSwaps[0])
			}
			assertBothReads()

			// State updates cannot replace or remove the original snapshot.
			if withSnapshot {
				swap.FundingTxs = nil
			} else {
				swap.FundingTxs = []*wire.MsgTx{parent}
			}
			swap.SetState(MonitorInvoiceAndHtlcTx)
			require.NoError(t, store.UpdateLoopIn(ctx, swap))
			assertBothReads()

			var encoded []byte
			err = db.QueryRowContext(ctx, `SELECT funding_txs
				FROM static_address_swaps WHERE swap_hash = $1`,
				swap.SwapHash[:]).Scan(&encoded)
			require.NoError(t, err)
			if !withSnapshot {
				require.Nil(t, encoded)
				return
			}
			var rawTxs [][]byte
			require.NoError(t, json.Unmarshal(encoded, &rawTxs))
			require.Equal(t, [][]byte{raw.Bytes()}, rawTxs)

			// Corrupt records fail both read paths instead of becoming
			// partial evidence authorizing a release after restart.
			withTrailingBytes, err := json.Marshal([][]byte{
				append(bytes.Clone(raw.Bytes()), 0),
			})
			require.NoError(t, err)
			duplicateInputs := parent.Copy()
			duplicateInputs.TxIn = append(
				duplicateInputs.TxIn, duplicateInputs.TxIn[0],
			)
			var duplicateRaw bytes.Buffer
			require.NoError(t, duplicateInputs.SerializeNoWitness(&duplicateRaw))
			withDuplicateInputs, err := json.Marshal([][]byte{duplicateRaw.Bytes()})
			require.NoError(t, err)
			for _, malformed := range [][]byte{
				[]byte("{"), []byte(`{}`), []byte(`["!"]`),
				[]byte(`[null]`), withTrailingBytes, withDuplicateInputs,
			} {
				_, err := db.ExecContext(ctx, `UPDATE static_address_swaps
					SET funding_txs = $1 WHERE swap_hash = $2`,
					malformed, swap.SwapHash[:])
				require.NoError(t, err)
				_, err = store.GetLoopInByHash(ctx, swap.SwapHash)
				require.Error(t, err)
				_, err = store.GetStaticAddressLoopInSwapsByStates(
					ctx, []fsm.StateType{swap.GetState()},
				)
				require.Error(t, err)
			}
		})
	}
}

// TestLoopInChangeAddressRoundTrip verifies that a generated per-swap change
// address survives both direct lookup and state-based recovery.
func TestLoopInChangeAddressRoundTrip(t *testing.T) {
	ctx := t.Context()
	testDB := loopdb.NewTestDB(t)
	defer testDB.Close()

	testClock := clock.NewTestClock(time.Now())
	depositStore := deposit.NewSqlStore(testDB.BaseDB)
	loopInStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDB), testClock,
		&chaincfg.RegressionNetParams,
	)
	addressStore := address.NewSqlStore(testDB.BaseDB)

	depositID, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	ownedDeposit := &deposit.Deposit{
		ID: depositID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 2,
		},
		Value:                100_000,
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x03},
	}
	setPersistedTestDepositAddress(
		t, ctx, testDB.BaseDB, ownedDeposit,
	)
	require.NoError(t, depositStore.CreateDeposit(ctx, ownedDeposit))
	ownedDeposit.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.UpdateDeposit(ctx, ownedDeposit))

	_, changeClientPubkey := test.CreateKey(1)
	_, changeServerPubkey := test.CreateKey(2)
	changeParams := &address.Parameters{
		ClientPubkey: changeClientPubkey,
		ServerPubkey: changeServerPubkey,
		Expiry:       288,
		KeyLocator: keychain.KeyLocator{
			Family: 321,
			Index:  654,
		},
		PkScript:         []byte{0x51, 0x20, 0x04},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 987,
	}
	require.NoError(
		t, addressStore.CreateStaticAddress(ctx, changeParams),
	)
	changeParams.ID, err = addressStore.GetStaticAddressID(
		ctx, changeParams.PkScript,
	)
	require.NoError(t, err)

	_, swapClientPubkey := test.CreateKey(3)
	_, swapServerPubkey := test.CreateKey(4)
	timeoutAddress, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{5, 6, 7, 8}
	swap := &StaticAddressLoopIn{
		SwapHash:                swapHash,
		SwapPreimage:            lntypes.Preimage{9, 10, 11, 12},
		DepositOutpoints:        []string{ownedDeposit.OutPoint.String()},
		Deposits:                []*deposit.Deposit{ownedDeposit},
		SelectedAmount:          60_000,
		ClientPubkey:            swapClientPubkey,
		ServerPubkey:            swapServerPubkey,
		HtlcTimeoutSweepAddress: timeoutAddress,
		ChangeAddressParams:     changeParams,
	}
	swap.SetState(SignHtlcTx)
	require.NoError(t, loopInStore.CreateLoopIn(ctx, swap))

	assertChangeAddress := func(t *testing.T,
		got *address.Parameters) {

		t.Helper()
		require.NotNil(t, got)
		require.Equal(t, changeParams.ID, got.ID)
		require.Equal(
			t, changeParams.ClientPubkey.SerializeCompressed(),
			got.ClientPubkey.SerializeCompressed(),
		)
		require.Equal(
			t, changeParams.ServerPubkey.SerializeCompressed(),
			got.ServerPubkey.SerializeCompressed(),
		)
		require.Equal(t, changeParams.Expiry, got.Expiry)
		require.Equal(t, changeParams.KeyLocator, got.KeyLocator)
		require.Equal(t, changeParams.PkScript, got.PkScript)
		require.Equal(
			t, changeParams.ProtocolVersion, got.ProtocolVersion,
		)
		require.Equal(
			t, changeParams.InitiationHeight, got.InitiationHeight,
		)
	}

	restoredSwap, err := loopInStore.GetLoopInByHash(ctx, swapHash)
	require.NoError(t, err)
	assertChangeAddress(t, restoredSwap.ChangeAddressParams)

	recoveredSwaps, err := loopInStore.GetStaticAddressLoopInSwapsByStates(
		ctx, []fsm.StateType{SignHtlcTx},
	)
	require.NoError(t, err)
	require.Len(t, recoveredSwaps, 1)
	assertChangeAddress(t, recoveredSwaps[0].ChangeAddressParams)
}

// TestLoopInDepositAddressOwnershipRoundTrip asserts that deposits restored as
// part of a loop-in retain the static address parameters needed for signing.
func TestLoopInDepositAddressOwnershipRoundTrip(t *testing.T) {
	ctx := context.Background()
	testDB := loopdb.NewTestDB(t)
	defer testDB.Close()

	testClock := clock.NewTestClock(time.Now())
	depositStore := deposit.NewSqlStore(testDB.BaseDB)
	loopInStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDB), testClock,
		&chaincfg.RegressionNetParams,
	)
	addressStore := address.NewSqlStore(testDB.BaseDB)

	_, addressClientPubkey := test.CreateKey(1)
	_, addressServerPubkey := test.CreateKey(2)
	addressParams := &script.Parameters{
		ClientPubkey: addressClientPubkey,
		ServerPubkey: addressServerPubkey,
		Expiry:       144,
		KeyLocator: keychain.KeyLocator{
			Family: 123,
			Index:  456,
		},
		PkScript:         []byte{0x51, 0x20, 0x02},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 789,
	}
	require.NoError(t, addressStore.CreateStaticAddress(ctx, addressParams))

	var err error
	addressParams.ID, err = addressStore.GetStaticAddressID(
		ctx, addressParams.PkScript,
	)
	require.NoError(t, err)

	depositID, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	ownedDeposit := &deposit.Deposit{
		ID: depositID,
		OutPoint: wire.OutPoint{
			Hash:  wire.NewMsgTx(2).TxHash(),
			Index: 3,
		},
		Value:                100_000,
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x03},
		AddressParams:        addressParams,
	}
	ownedDeposit.SetState(deposit.Deposited)
	require.NoError(t, depositStore.CreateDeposit(ctx, ownedDeposit))

	ownedDeposit.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.UpdateDeposit(ctx, ownedDeposit))

	_, swapClientPubkey := test.CreateKey(3)
	_, swapServerPubkey := test.CreateKey(4)
	timeoutAddress, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x01, 0x02, 0x03, 0x04}
	swap := &StaticAddressLoopIn{
		SwapHash:                swapHash,
		SwapPreimage:            lntypes.Preimage{0x05, 0x06, 0x07, 0x08},
		DepositOutpoints:        []string{ownedDeposit.OutPoint.String()},
		Deposits:                []*deposit.Deposit{ownedDeposit},
		ClientPubkey:            swapClientPubkey,
		ServerPubkey:            swapServerPubkey,
		HtlcTimeoutSweepAddress: timeoutAddress,
	}
	swap.SetState(SignHtlcTx)
	require.NoError(t, loopInStore.CreateLoopIn(ctx, swap))

	restoredSwap, err := loopInStore.GetLoopInByHash(ctx, swapHash)
	require.NoError(t, err)
	require.Len(t, restoredSwap.Deposits, 1)

	restoredParams := restoredSwap.Deposits[0].AddressParams
	require.NotNil(t, restoredParams)
	require.Equal(t, addressParams.ID, restoredParams.ID)
	require.Equal(
		t, addressParams.ClientPubkey.SerializeCompressed(),
		restoredParams.ClientPubkey.SerializeCompressed(),
	)
	require.Equal(
		t, addressParams.ServerPubkey.SerializeCompressed(),
		restoredParams.ServerPubkey.SerializeCompressed(),
	)
	require.Equal(t, addressParams.Expiry, restoredParams.Expiry)
	require.Equal(t, addressParams.KeyLocator, restoredParams.KeyLocator)
	require.Equal(t, addressParams.PkScript, restoredParams.PkScript)
	require.Equal(t, addressParams.ProtocolVersion,
		restoredParams.ProtocolVersion)
	require.Equal(t, addressParams.InitiationHeight,
		restoredParams.InitiationHeight)
}

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

	setPersistedTestDepositAddress(
		t, ctxb, testDb.BaseDB, d1, d2, d3, d4,
	)

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

	// Store a known on-chain cost for the looped-in deposit only, to check
	// that it is loaded with the swap's deposits.
	d3Cost := deposit.OnchainCost{
		Amount: 900,
		Status: deposit.OnchainCostKnown,
	}
	err = depositStore.SetOnchainCosts(
		ctxb, map[deposit.ID]deposit.OnchainCost{d3.ID: d3Cost},
	)
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
	require.Equal(t, deposit.OnchainCost{}, pendingDeposits[0].OnchainCost)

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
	require.Equal(t, d3Cost, finalizedDeposits[0].OnchainCost)
}

// TestCreateLoopIn tests that CreateLoopIn correctly creates a new
// StaticAddressLoopIn swap and associates it with the provided deposits.
func TestCreateLoopIn(t *testing.T) {
	// Set up test context objects.
	ctx := t.Context()
	testDb := loopdb.NewTestDB(t)
	createTime := time.Unix(1_717_171_717, 123_456_789).UTC()
	expectedCreateTime := createTime.Truncate(time.Microsecond)
	testClock := clock.NewTestClock(createTime)
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

	setPersistedTestDepositAddress(t, ctx, testDb.BaseDB, d1, d2)

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
	require.Equal(t, expectedCreateTime, swapPending.LastUpdateTime)

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

	// Deposits without a swap hash or without a database row must not hide
	// the swap of the remaining deposits.
	unmapped := &deposit.Deposit{
		ID: newID(),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x2a, 0x2b, 0x3c, 0x4f},
			Index: 2,
		},
		Value: btcutil.Amount(300_000),
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4e,
		},
		AddressParams: d1.AddressParams,
	}
	err = depositStore.CreateDeposit(ctx, unmapped)
	require.NoError(t, err)

	swapHashes, err = swapStore.SwapHashesForDepositIDs(
		ctx, []deposit.ID{
			unmapped.ID, newID(), depositIDs[0], depositIDs[1],
		},
	)
	require.NoError(t, err)
	require.Len(t, swapHashes, 1)
	require.ElementsMatch(
		t, []deposit.ID{depositIDs[0], depositIDs[1]},
		swapHashes[swapHashPending],
	)

	swap, err := swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(t, swapHashPending, swap.SwapHash)
	require.Equal(t, []string{d1.OutPoint.String(), d2.OutPoint.String()},
		swap.DepositOutpoints)
	require.Equal(t, SignHtlcTx, swap.GetState())
	require.Equal(t, swapPending.LastUpdateTime, swap.LastUpdateTime)
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

	// Replaying the same decision must retain its original deadline anchor.
	laterDecisionTime := decisionTime.Add(time.Hour)
	testClock.SetTime(laterDecisionTime)
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

	// A different decision is a new event and receives a new timestamp.
	rejectedDecisionTime := laterDecisionTime.Add(time.Hour)
	testClock.SetTime(rejectedDecisionTime)
	err = swapStore.RecordStaticAddressRiskDecision(
		ctx, swapHashPending, ConfirmationRiskDecisionRejected,
	)
	require.NoError(t, err)

	swap, err = swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(
		t, ConfirmationRiskDecisionRejected,
		swap.ConfirmationRiskDecision,
	)
	require.True(t, swap.ConfirmationRiskDecisionTime.Equal(
		rejectedDecisionTime,
	))

	// Rejected is terminal: a racing synthetic acceptance must not replace
	// the server's rejection or move its deadline anchor.
	testClock.SetTime(rejectedDecisionTime.Add(time.Hour))
	err = swapStore.RecordStaticAddressRiskDecision(
		ctx, swapHashPending, ConfirmationRiskDecisionAccepted,
	)
	require.NoError(t, err)

	swap, err = swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(
		t, ConfirmationRiskDecisionRejected,
		swap.ConfirmationRiskDecision,
	)
	require.True(t, swap.ConfirmationRiskDecisionTime.Equal(
		rejectedDecisionTime,
	))

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
	require.Equal(
		t, updateTime.Truncate(time.Microsecond),
		swapPending.LastUpdateTime,
	)

	swap, err = swapStore.GetLoopInByHash(ctx, swapHashPending)
	require.NoError(t, err)
	require.Equal(t, Succeeded, swap.GetState())
	require.Equal(t, swapPending.LastUpdateTime, swap.LastUpdateTime)
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

	setPersistedTestDepositAddress(t, ctx, testDb.BaseDB, d1, d2)

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
	setPersistedTestDepositAddress(t, ctxb, testDb.BaseDB, d)
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

// TestSqlStoreDepositsForSwap tests that DepositsForSwap returns the deposits
// mapped to a swap with their on-chain cost, and no deposit that a later swap
// took.
func TestSqlStoreDepositsForSwap(t *testing.T) {
	ctx := t.Context()
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb),
		clock.NewTestClock(time.Now()), &chaincfg.RegressionNetParams,
	)

	knownCost := deposit.OnchainCost{
		Amount: 1_550, Status: deposit.OnchainCostKnown,
	}
	newDeposit := func(seed byte,
		cost deposit.OnchainCost) *deposit.Deposit {

		id, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		d := &deposit.Deposit{
			ID: id,
			OutPoint: wire.OutPoint{
				Hash: chainhash.Hash{seed},
			},
			Value:                100_000,
			TimeOutSweepPkScript: []byte{0x00, 0x14, seed},
			OnchainCost:          cost,
		}
		require.NoError(t, depositStore.CreateDeposit(ctx, d))

		d.SetState(deposit.LoopingIn)
		require.NoError(t, depositStore.UpdateDeposit(ctx, d))

		return d
	}
	known := newDeposit(1, knownCost)
	taken := newDeposit(2, deposit.OnchainCost{})

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	createSwap := func(seed byte,
		deposits ...*deposit.Deposit) lntypes.Hash {

		outpoints := make([]string, 0, len(deposits))
		for _, d := range deposits {
			outpoints = append(outpoints, d.OutPoint.String())
		}

		swap := StaticAddressLoopIn{
			SwapHash:                lntypes.Hash{seed},
			SwapPreimage:            lntypes.Preimage{seed},
			DepositOutpoints:        outpoints,
			Deposits:                deposits,
			ClientPubkey:            clientPubKey,
			ServerPubkey:            serverPubKey,
			HtlcTimeoutSweepAddress: addr,
		}
		swap.SetState(SignHtlcTx)
		require.NoError(t, swapStore.CreateLoopIn(ctx, &swap))

		return swap.SwapHash
	}

	// The first swap tries both deposits, then a later swap takes one of
	// them.
	first := createSwap(1, known, taken)
	later := createSwap(2, taken)

	deposits, err := swapStore.DepositsForSwap(ctx, first)
	require.NoError(t, err)
	require.Len(t, deposits, 1)
	require.Equal(t, known.ID, deposits[0].ID)
	require.Equal(t, knownCost, deposits[0].GetOnchainCost())
	require.True(t, deposits[0].IsInState(deposit.LoopingIn))

	deposits, err = swapStore.DepositsForSwap(ctx, later)
	require.NoError(t, err)
	require.Len(t, deposits, 1)
	require.Equal(t, taken.ID, deposits[0].ID)
	require.Equal(t, deposit.OnchainCost{}, deposits[0].GetOnchainCost())

	deposits, err = swapStore.DepositsForSwap(ctx, lntypes.Hash{3})
	require.NoError(t, err)
	require.Empty(t, deposits)
}

// TestSqlStoreFailedSwapOnchainCost tests that a failed swap that published
// its htlc timeout sweep still reports the on-chain cost of its deposits after
// it is loaded from the store, and a failed swap without a sweep doesn't.
func TestSqlStoreFailedSwapOnchainCost(t *testing.T) {
	ctx := t.Context()
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	testClock := clock.NewTestClock(time.Now())
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	knownCost := deposit.OnchainCost{
		Amount: 1_550, Status: deposit.OnchainCostKnown,
	}

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	// createFailedSwap stores a swap whose deposit is on the htlc timeout
	// path and which then failed, with the given htlc timeout sweep.
	createFailedSwap := func(seed byte,
		sweepTxHash *chainhash.Hash) lntypes.Hash {

		id, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		d := &deposit.Deposit{
			ID: id,
			OutPoint: wire.OutPoint{
				Hash: chainhash.Hash{seed},
			},
			Value:                100_000,
			TimeOutSweepPkScript: []byte{0x00, 0x14, seed},
			OnchainCost:          knownCost,
		}
		require.NoError(t, depositStore.CreateDeposit(ctx, d))

		swap := StaticAddressLoopIn{
			SwapHash:                lntypes.Hash{seed},
			SwapPreimage:            lntypes.Preimage{seed},
			DepositOutpoints:        []string{d.OutPoint.String()},
			Deposits:                []*deposit.Deposit{d},
			ClientPubkey:            clientPubKey,
			ServerPubkey:            serverPubKey,
			HtlcTimeoutSweepAddress: addr,
		}
		swap.SetState(SignHtlcTx)
		require.NoError(t, swapStore.CreateLoopIn(ctx, &swap))

		d.SetState(deposit.SweepHtlcTimeout)
		require.NoError(t, depositStore.UpdateDeposit(ctx, d))

		// The latest update is the one with the latest timestamp.
		testClock.SetTime(testClock.Now().Add(time.Second))

		swap.HtlcTimeoutSweepTxHash = sweepTxHash
		swap.SetState(Failed)
		require.NoError(t, swapStore.UpdateLoopIn(ctx, &swap))

		return swap.SwapHash
	}

	swept := createFailedSwap(1, &chainhash.Hash{9})
	notSwept := createFailedSwap(2, nil)

	swaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctx, AllStates,
	)
	require.NoError(t, err)
	require.Len(t, swaps, 2)

	costs := make(map[lntypes.Hash]OnchainCost, len(swaps))
	for _, swp := range swaps {
		deposits, err := swapStore.DepositsForSwap(ctx, swp.SwapHash)
		require.NoError(t, err)

		costs[swp.SwapHash] = SwapOnchainCost(swp, deposits)
	}

	require.Equal(t, OnchainCost{
		Amount: 1_550, Status: OnchainCostComplete,
	}, costs[swept])
	require.Equal(
		t, OnchainCost{Status: OnchainCostUnknown}, costs[notSwept],
	)
}

// TestLatestLoopInUpdateIsLastInserted tests that a swap's current state is
// its last inserted update, also if an earlier update has a later timestamp,
// as happens for updates written in local time east of UTC before an upgrade
// that writes them in UTC.
func TestLatestLoopInUpdateIsLastInserted(t *testing.T) {
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	testClock := clock.NewTestClock(now)

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	depositID, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	d := &deposit.Deposit{
		ID:                   depositID,
		Hash:                 chainhash.Hash{2},
		Index:                0,
		Value:                btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x01},
	}
	setPersistedTestDepositAddress(t, ctx, testDb.BaseDB, d)
	d.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.CreateDeposit(ctx, d))

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		SwapHash:                lntypes.Hash{2},
		SwapPreimage:            lntypes.Preimage{2},
		InitiationTime:          now,
		DepositOutpoints:        []string{d.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	loopIn.SetState(SignHtlcTx)
	require.NoError(t, swapStore.CreateLoopIn(ctx, loopIn))
	createdID := loopIn.LastUpdateID
	require.Positive(t, createdID)

	// An update written before the upgrade in local time two hours east of
	// UTC has a timestamp two hours later than the updates after it.
	_, err = testDb.BaseDB.ExecContext(
		ctx, "INSERT INTO static_address_swap_updates (swap_hash, "+
			"update_state, update_timestamp) VALUES ($1, $2, $3)",
		loopIn.SwapHash[:], string(MonitorInvoiceAndHtlcTx),
		now.Add(2*time.Hour),
	)
	require.NoError(t, err)

	// The swap succeeds after the upgrade.
	testClock.SetTime(now.Add(time.Minute))
	loopIn.SetState(Succeeded)
	require.NoError(t, swapStore.UpdateLoopIn(ctx, loopIn))

	// The swap's update ids increase, unlike its update times.
	require.Greater(t, loopIn.LastUpdateID, createdID+1)

	pending, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctx, PendingStates,
	)
	require.NoError(t, err)
	require.Empty(t, pending)

	final, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctx, FinalStates,
	)
	require.NoError(t, err)
	require.Len(t, final, 1)
	require.Equal(t, Succeeded, final[0].GetState())
	require.Equal(t, loopIn.LastUpdateID, final[0].LastUpdateID)

	loaded, err := swapStore.GetLoopInByHash(ctx, loopIn.SwapHash)
	require.NoError(t, err)
	require.Equal(t, Succeeded, loaded.GetState())
	require.True(t, now.Add(time.Minute).Equal(loaded.LastUpdateTime))
	require.Equal(t, loopIn.LastUpdateID, loaded.LastUpdateID)
}

// TestSqlStoreTimesOutsideUTC tests that the times the store writes read back
// unchanged when the clock isn't in UTC. A Postgres TIMESTAMP column drops the
// time zone, so a time written in local time would read back shifted by the
// offset.
func TestSqlStoreTimesOutsideUTC(t *testing.T) {
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	ctx := t.Context()
	newYork := time.FixedZone("EDT", -4*60*60)
	now := time.Now().In(newYork).Truncate(time.Microsecond)

	testClock := clock.NewTestClock(now)
	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	depositID, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	d := &deposit.Deposit{
		ID:                   depositID,
		Hash:                 chainhash.Hash{1},
		Index:                0,
		Value:                btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x01},
	}
	setPersistedTestDepositAddress(t, ctx, testDb.BaseDB, d)
	d.SetState(deposit.LoopingIn)
	require.NoError(t, depositStore.CreateDeposit(ctx, d))

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		SwapHash:                lntypes.Hash{1},
		SwapPreimage:            lntypes.Preimage{1},
		InitiationTime:          now,
		DepositOutpoints:        []string{d.OutPoint.String()},
		Deposits:                []*deposit.Deposit{d},
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)
	require.NoError(t, swapStore.CreateLoopIn(ctx, loopIn))

	err = swapStore.RecordStaticAddressRiskDecision(
		ctx, loopIn.SwapHash, ConfirmationRiskDecisionAccepted,
	)
	require.NoError(t, err)

	loaded, err := swapStore.GetLoopInByHash(ctx, loopIn.SwapHash)
	require.NoError(t, err)
	require.True(t, now.Equal(loaded.InitiationTime),
		"initiation time %v, want %v", loaded.InitiationTime, now)
	require.True(t, now.Equal(loaded.ConfirmationRiskDecisionTime),
		"risk decision time %v, want %v",
		loaded.ConfirmationRiskDecisionTime, now)
	require.True(t, now.Equal(loaded.LastUpdateTime),
		"update time %v, want %v", loaded.LastUpdateTime, now)

	later := now.Add(time.Minute)
	testClock.SetTime(later)
	loopIn.SetState(Succeeded)
	require.NoError(t, swapStore.UpdateLoopIn(ctx, loopIn))

	loaded, err = swapStore.GetLoopInByHash(ctx, loopIn.SwapHash)
	require.NoError(t, err)
	require.True(t, later.Equal(loaded.LastUpdateTime),
		"update time %v, want %v", loaded.LastUpdateTime, later)
}
