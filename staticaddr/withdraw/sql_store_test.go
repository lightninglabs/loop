package withdraw

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestSqlStore tests the basic functionality of the SQLStore.
func TestSqlStore(t *testing.T) {
	ctxb := t.Context()
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	store := NewSqlStore(loopdb.NewTypedStore[Querier](testDb), depositStore)

	newID := func() deposit.ID {
		did, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		return did
	}

	d1, d2 := &deposit.Deposit{
		ID:    newID(),
		Value: btcutil.Amount(100_000),
		TimeOutSweepPkScript: []byte{
			0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x41,
		},
	},
		&deposit.Deposit{
			ID:    newID(),
			Value: btcutil.Amount(50_000),
			TimeOutSweepPkScript: []byte{
				0x00, 0x14, 0x1a, 0x2b, 0x3c, 0x4d,
			},
		}
	_, clientPubkey := test.CreateKey(101)
	_, serverPubkey := test.CreateKey(102)
	addressParams := &script.Parameters{
		ClientPubkey: clientPubkey,
		ServerPubkey: serverPubkey,
		Expiry:       144,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticSingleAddressKeyFamily),
			Index:  456,
		},
		PkScript:         []byte{0x51, 0x20, 0x01},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 789,
	}
	addressStore := address.NewSqlStore(testDb.BaseDB)
	err := addressStore.CreateStaticAddress(ctxb, addressParams)
	require.NoError(t, err)
	addressParams.ID, err = addressStore.GetStaticAddressID(
		ctxb, addressParams.PkScript,
	)
	require.NoError(t, err)
	d1.AddressParams = addressParams
	d2.AddressParams = addressParams

	withdrawalTx := &wire.MsgTx{
		Version: 2,
		TxOut: []*wire.TxOut{
			{
				Value: 100,
				PkScript: []byte{
					0x01,
				},
			},
			{
				Value: 100_000,
				PkScript: []byte{
					0x00,
				},
			},
			{
				Value: int64(d1.Value + d2.Value - 100 - 100_000),
				PkScript: []byte{
					0x02,
				},
			},
		},
	}

	err = depositStore.CreateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d2)
	require.NoError(t, err)

	err = store.CreateWithdrawal(ctxb, []*deposit.Deposit{d1, d2})
	require.NoError(t, err)

	withdrawals, err := store.GetAllWithdrawals(ctxb)
	require.NoError(t, err)
	require.Len(t, withdrawals, 1)
	require.NotEmpty(t, withdrawals[0].ID)
	require.EqualValues(
		t, d1.Value+d2.Value, withdrawals[0].TotalDepositAmount,
	)
	require.Len(t, withdrawals[0].Deposits, 2)
	require.EqualValues(
		t, d1.Value, withdrawals[0].Deposits[0].Value,
	)
	require.EqualValues(
		t, d2.Value, withdrawals[0].Deposits[1].Value,
	)
	require.NotEmpty(t, withdrawals[0].InitiationTime)

	rootScript := addressParams.PkScript
	withdrawalScript := []byte{0x00}
	changeScript := []byte{0x01}
	receiveScript := []byte{0x03}
	unknownScript := []byte{0x04}
	addrManager := &withdrawalTestAddressManager{
		params: map[string]*address.AddressParameters{
			string(rootScript): addressParams,
			string(changeScript): {
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(swap.StaticAddressChangeKeyFamily),
				},
			},
			string(receiveScript): {
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(swap.StaticMultiAddressKeyFamily),
				},
			},
		},
	}
	txOut := func(value int64, pkScript []byte) *wire.TxOut {
		return &wire.TxOut{Value: value, PkScript: pkScript}
	}

	// Exercise change identification and SQL persistence together, as when
	// an in-flight withdrawal confirms after an upgrade.
	testCases := []struct {
		name             string
		outputs          []*wire.TxOut
		withdrawalScript []byte
		withdrawnAmount  int64
		changeAmount     int64
	}{
		{
			name:             "derived change with extra withdrawal output",
			outputs:          withdrawalTx.TxOut,
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  int64(d1.Value+d2.Value) - 100,
			changeAmount:     100,
		},
		{
			name: "legacy root change",
			outputs: []*wire.TxOut{
				txOut(100_000, withdrawalScript), txOut(49_000, rootScript),
			},
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  100_000,
			changeAmount:     49_000,
		},
		{
			name: "legacy root change first",
			outputs: []*wire.TxOut{
				txOut(49_000, rootScript), txOut(100_000, withdrawalScript),
			},
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  100_000,
			changeAmount:     49_000,
		},
		{
			name: "legacy root destination and change",
			outputs: []*wire.TxOut{
				txOut(100_000, rootScript), txOut(49_000, rootScript),
			},
			withdrawalScript: rootScript,
			withdrawnAmount:  100_000,
			changeAmount:     49_000,
		},
		{
			name:             "full withdrawal to root",
			outputs:          []*wire.TxOut{txOut(149_000, rootScript)},
			withdrawalScript: rootScript,
			withdrawnAmount:  149_000,
		},
		{
			name: "derived change",
			outputs: []*wire.TxOut{
				txOut(100_000, withdrawalScript), txOut(49_000, changeScript),
			},
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  100_000,
			changeAmount:     49_000,
		},
		{
			name: "root destination with derived change",
			outputs: []*wire.TxOut{
				txOut(100_000, rootScript), txOut(49_000, changeScript),
			},
			withdrawalScript: rootScript,
			withdrawnAmount:  100_000,
			changeAmount:     49_000,
		},
		{
			name: "receive destination with derived change",
			outputs: []*wire.TxOut{
				txOut(100_000, receiveScript), txOut(49_000, changeScript),
			},
			withdrawalScript: receiveScript,
			withdrawnAmount:  100_000,
			changeAmount:     49_000,
		},
		{
			name: "receive output is not change",
			outputs: []*wire.TxOut{
				txOut(100_000, withdrawalScript), txOut(49_000, receiveScript),
			},
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  149_000,
		},
		{
			name: "unknown output is not change",
			outputs: []*wire.TxOut{
				txOut(100_000, withdrawalScript), txOut(49_000, unknownScript),
			},
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  149_000,
		},
		{
			name:             "full withdrawal",
			outputs:          []*wire.TxOut{txOut(149_000, withdrawalScript)},
			withdrawalScript: withdrawalScript,
			withdrawnAmount:  149_000,
		},
		{
			name: "root destination with unknown second output",
			outputs: []*wire.TxOut{
				txOut(100_000, rootScript), txOut(49_000, unknownScript),
			},
			withdrawalScript: rootScript,
			withdrawnAmount:  149_000,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tx := wire.NewMsgTx(2)
			tx.TxOut = testCase.outputs
			changePkScript, err := withdrawalChangePkScript(
				tx, testCase.withdrawalScript, addrManager,
			)
			require.NoError(t, err)

			err = store.UpdateWithdrawal(
				t.Context(), []*deposit.Deposit{d1, d2}, tx, 6,
				changePkScript,
			)
			require.NoError(t, err)

			withdrawals, err := store.GetAllWithdrawals(t.Context())
			require.NoError(t, err)
			require.Len(t, withdrawals, 1)
			require.Equal(t, tx.TxHash(), withdrawals[0].TxID)
			require.EqualValues(t, testCase.withdrawnAmount,
				withdrawals[0].WithdrawnAmount)
			require.EqualValues(t, testCase.changeAmount,
				withdrawals[0].ChangeAmount)
			require.EqualValues(t, 6, withdrawals[0].ConfirmationHeight)
		})
	}
}
