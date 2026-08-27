package withdraw

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestSqlStore tests the basic functionality of the SQLStore.
func TestSqlStore(t *testing.T) {
	ctxb := context.Background()
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
			Value: btcutil.Amount(200_000),
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
			Family: 123,
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

	err = store.UpdateWithdrawal(
		ctxb, []*deposit.Deposit{d1, d2}, withdrawalTx, 6, []byte{0x01},
	)
	require.NoError(t, err)

	withdrawals, err = store.GetAllWithdrawals(ctxb)
	require.NoError(t, err)
	require.Len(t, withdrawals, 1)
	require.NotEmpty(t, withdrawals[0].TxID)
	require.EqualValues(
		t, d1.Value+d2.Value-100, withdrawals[0].WithdrawnAmount,
	)
	require.EqualValues(t, 100, withdrawals[0].ChangeAmount)
	require.EqualValues(t, 6, withdrawals[0].ConfirmationHeight)
}
