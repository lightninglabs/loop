package staticaddr_test

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/staticaddr/withdraw"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestMultiAddressPersistenceRecovery exercises the shared SQL boundary used
// by address issuance, deposits, fractional loop-ins and withdrawals. It then
// reconstructs every store to model a loopd restart and verifies that each
// deposit retains its owning address while change uses a separate address.
func TestMultiAddressPersistenceRecovery(t *testing.T) {
	ctx := t.Context()
	db := loopdb.NewTestDB(t)
	t.Cleanup(func() {
		db.Close()
	})

	addressStore := address.NewSqlStore(db.BaseDB)
	receiveA := createIntegrationAddress(
		t, ctx, addressStore, 1, 11, 1,
	)
	receiveB := createIntegrationAddress(
		t, ctx, addressStore, 2, 12, 2,
	)
	loopInChange := createIntegrationAddress(
		t, ctx, addressStore, 3, 13, 3,
	)
	withdrawChange := createIntegrationAddress(
		t, ctx, addressStore, 4, 14, 4,
	)
	require.NotEqual(t, loopInChange.ID, withdrawChange.ID)
	require.NotEqual(t, loopInChange.PkScript, withdrawChange.PkScript)

	depositStore := deposit.NewSqlStore(db.BaseDB)
	loopInA := createIntegrationDeposit(
		t, ctx, depositStore, 1, 300_000, receiveA,
	)
	loopInB := createIntegrationDeposit(
		t, ctx, depositStore, 2, 250_000, receiveB,
	)
	withdrawA := createIntegrationDeposit(
		t, ctx, depositStore, 3, 200_000, receiveA,
	)
	withdrawB := createIntegrationDeposit(
		t, ctx, depositStore, 4, 300_000, receiveB,
	)

	_, htlcClientKey := test.CreateKey(21)
	_, htlcServerKey := test.CreateKey(22)
	timeoutAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	swapHash := lntypes.Hash{1, 2, 3}
	swap := &loopin.StaticAddressLoopIn{
		SwapHash:            swapHash,
		SwapPreimage:        lntypes.Preimage{1, 2, 3},
		DepositOutpoints:    []string{loopInA.String(), loopInB.String()},
		Deposits:            []*deposit.Deposit{loopInA, loopInB},
		SelectedAmount:      400_000,
		ChangeAddressParams: loopInChange,
		ClientPubkey:        htlcClientKey,
		ServerPubkey:        htlcServerKey,
		HtlcKeyLocator: keychain.KeyLocator{
			Family: 44,
			Index:  1,
		},
		HtlcTimeoutSweepAddress: timeoutAddr,
		InitiationHeight:        100,
		InitiationTime:          time.Unix(1_700_000_000, 0),
	}
	swap.SetState(loopin.SignHtlcTx)
	loopInStore := loopin.NewSqlStore(
		loopdb.NewTypedStore[loopin.Querier](db),
		clock.NewTestClock(time.Unix(1_700_000_001, 0)),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, loopInStore.CreateLoopIn(ctx, swap))

	withdrawStore := withdraw.NewSqlStore(
		loopdb.NewTypedStore[withdraw.Querier](db), depositStore,
	)
	withdrawDeposits := []*deposit.Deposit{withdrawA, withdrawB}
	require.NoError(
		t, withdrawStore.CreateWithdrawal(ctx, withdrawDeposits),
	)

	replacementTx := wire.NewMsgTx(2)
	replacementTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: withdrawA.OutPoint,
	})
	replacementTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: withdrawB.OutPoint,
	})
	replacementTx.AddTxOut(&wire.TxOut{
		Value:    425_000,
		PkScript: []byte{0x51},
	})
	replacementTx.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: withdrawChange.PkScript,
	})
	require.NoError(t, withdrawStore.UpdateWithdrawal(
		ctx, withdrawDeposits, replacementTx, 110,
		withdrawChange.PkScript,
	))

	// Recreate the stores to exercise the same read path used after restart.
	restartedDepositStore := deposit.NewSqlStore(db.BaseDB)
	restartedLoopInStore := loopin.NewSqlStore(
		loopdb.NewTypedStore[loopin.Querier](db),
		clock.NewTestClock(time.Unix(1_700_000_002, 0)),
		&chaincfg.RegressionNetParams,
	)
	recoveredSwap, err := restartedLoopInStore.GetLoopInByHash(ctx, swapHash)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(400_000), recoveredSwap.SelectedAmount)
	require.NotNil(t, recoveredSwap.ChangeAddressParams)
	require.Equal(
		t, loopInChange.ID, recoveredSwap.ChangeAddressParams.ID,
	)
	requireDepositAddressIDs(t, recoveredSwap.Deposits, map[string]int32{
		loopInA.String(): receiveA.ID,
		loopInB.String(): receiveB.ID,
	})

	restartedWithdrawStore := withdraw.NewSqlStore(
		loopdb.NewTypedStore[withdraw.Querier](db),
		restartedDepositStore,
	)
	recoveredWithdrawals, err := restartedWithdrawStore.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, recoveredWithdrawals, 1)
	require.Equal(
		t, replacementTx.TxHash(), recoveredWithdrawals[0].TxID,
	)
	require.Equal(
		t, btcutil.Amount(50_000), recoveredWithdrawals[0].ChangeAmount,
	)
	requireDepositAddressIDs(
		t, recoveredWithdrawals[0].Deposits, map[string]int32{
			withdrawA.String(): receiveA.ID,
			withdrawB.String(): receiveB.ID,
		},
	)
}

func createIntegrationAddress(t *testing.T, ctx context.Context,
	store *address.SqlStore, clientIndex, serverIndex byte,
	keyIndex uint32) *address.Parameters {

	t.Helper()

	_, clientKey := test.CreateKey(int32(clientIndex))
	_, serverKey := test.CreateKey(int32(serverIndex))
	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, 1_000, clientKey, serverKey,
	)
	require.NoError(t, err)
	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	params := &address.Parameters{
		ClientPubkey: clientKey,
		ServerPubkey: serverKey,
		PkScript:     pkScript,
		Expiry:       1_000,
		KeyLocator: keychain.KeyLocator{
			Family: 99,
			Index:  keyIndex,
		},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 100,
	}
	require.NoError(t, store.CreateStaticAddress(ctx, params))
	params.ID, err = store.GetStaticAddressID(ctx, params.PkScript)
	require.NoError(t, err)

	return params
}

func createIntegrationDeposit(t *testing.T, ctx context.Context,
	store *deposit.SqlStore, hashByte byte, value btcutil.Amount,
	params *address.Parameters) *deposit.Deposit {

	t.Helper()

	id, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	d := &deposit.Deposit{
		ID: id,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{hashByte},
			Index: uint32(hashByte),
		},
		Value:                value,
		ConfirmationHeight:   90,
		TimeOutSweepPkScript: []byte{0x00, 0x14, hashByte},
		AddressParams:        params,
	}
	d.SetState(deposit.Deposited)
	require.NoError(t, store.CreateDeposit(ctx, d))
	require.NoError(t, store.UpdateDeposit(ctx, d))

	return d
}

func requireDepositAddressIDs(t *testing.T, deposits []*deposit.Deposit,
	want map[string]int32) {

	t.Helper()
	require.Len(t, deposits, len(want))
	for _, d := range deposits {
		require.NotNil(t, d.AddressParams)
		require.Equal(t, want[d.String()], d.AddressParams.ID)
	}
}
