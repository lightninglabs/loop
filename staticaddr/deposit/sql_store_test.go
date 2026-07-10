package deposit

import (
	"context"
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v5"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func TestCreateDepositRejectsUnpersistedAddress(t *testing.T) {
	store := NewSqlStore(nil)
	deposit := &Deposit{
		AddressParams: &script.Parameters{},
	}

	err := store.CreateDeposit(context.Background(), deposit)
	require.ErrorContains(t, err, "static address ID must be set")
}

// TestDepositAddressOwnershipRoundTrip asserts that every deposit read path
// restores the static address parameters referenced by the deposit row.
func TestDepositAddressOwnershipRoundTrip(t *testing.T) {
	ctx := context.Background()
	testDB := loopdb.NewTestDB(t)
	defer testDB.Close()

	addressStore := address.NewSqlStore(testDB.BaseDB)
	_, clientPubkey := test.CreateKey(1)
	_, serverPubkey := test.CreateKey(2)
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

	err := addressStore.CreateStaticAddress(ctx, addressParams)
	require.NoError(t, err)
	addressParams.ID, err = addressStore.GetStaticAddressID(
		ctx, addressParams.PkScript,
	)
	require.NoError(t, err)

	depositID, err := GetRandomDepositID()
	require.NoError(t, err)

	deposit := &Deposit{
		ID: depositID,
		OutPoint: wire.OutPoint{
			Hash:  wire.NewMsgTx(2).TxHash(),
			Index: 3,
		},
		Value:                100_000,
		ConfirmationHeight:   321,
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x02},
		AddressParams:        addressParams,
		state:                Deposited,
	}

	store := NewSqlStore(testDB.BaseDB)
	require.NoError(t, store.CreateDeposit(ctx, deposit))

	assertOwnership := func(t *testing.T, restored *Deposit) {
		t.Helper()
		require.NotNil(t, restored.AddressParams)
		require.Equal(t, addressParams.ID, restored.AddressParams.ID)
		require.Equal(
			t, addressParams.ClientPubkey.SerializeCompressed(),
			restored.AddressParams.ClientPubkey.SerializeCompressed(),
		)
		require.Equal(
			t, addressParams.ServerPubkey.SerializeCompressed(),
			restored.AddressParams.ServerPubkey.SerializeCompressed(),
		)
		require.Equal(t, addressParams.Expiry,
			restored.AddressParams.Expiry)
		require.Equal(t, addressParams.KeyLocator,
			restored.AddressParams.KeyLocator)
		require.Equal(t, addressParams.PkScript,
			restored.AddressParams.PkScript)
		require.Equal(t, addressParams.ProtocolVersion,
			restored.AddressParams.ProtocolVersion)
		require.Equal(t, addressParams.InitiationHeight,
			restored.AddressParams.InitiationHeight)
	}

	restored, err := store.GetDeposit(ctx, depositID)
	require.NoError(t, err)
	assertOwnership(t, restored)

	restored, err = store.DepositForOutpoint(ctx, deposit.OutPoint.String())
	require.NoError(t, err)
	assertOwnership(t, restored)

	allDeposits, err := store.AllDeposits(ctx)
	require.NoError(t, err)
	require.Len(t, allDeposits, 1)
	assertOwnership(t, allDeposits[0])
}

func TestToDeposit(t *testing.T) {
	depositID, err := GetRandomDepositID()
	require.NoError(t, err)

	swapHash, err := lntypes.MakeHash(dummyHashBytes())
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	txHash := tx.TxHash()

	tests := []struct {
		name       string
		row        sqlc.AllDepositsRow
		lastUpdate sqlc.DepositUpdate
		expectErr  bool
	}{
		{
			name: "fully valid data",
			row: sqlc.AllDepositsRow{
				DepositID:          depositID[:],
				TxHash:             txHash[:],
				Amount:             100000000,
				ConfirmationHeight: 123456,
				SwapHash:           swapHash[:],
			},
			lastUpdate: sqlc.DepositUpdate{
				UpdateState: "completed",
			},
			expectErr: false,
		},
		{
			name: "fully valid data",
			row: sqlc.AllDepositsRow{
				DepositID:          depositID[:],
				TxHash:             txHash[:],
				Amount:             100000000,
				ConfirmationHeight: 123456,
			},
			lastUpdate: sqlc.DepositUpdate{
				UpdateState: "completed",
			},
			expectErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ToDeposit(test.row, test.lastUpdate)
			if test.expectErr {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, fsm.StateType(test.lastUpdate.UpdateState), result.state)
			}
		})
	}
}

func dummyHashBytes() []byte {
	return []byte{0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
		0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15,
		0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
		0x20, 0x21, 0x22, 0x23}
}

// TestErrNoRows ensures that pgx.ErrNoRows is a wrapped sql.ErrNoRows, so we
// don't have to check against both of them.
func TestErrNoRows(t *testing.T) {
	require.ErrorIs(t, pgx.ErrNoRows, sql.ErrNoRows)
}
