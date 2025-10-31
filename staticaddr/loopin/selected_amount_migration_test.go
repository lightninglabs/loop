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
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestMigrateSelectedSwapAmount tests the selected amount migration.
func TestMigrateSelectedSwapAmount(t *testing.T) {
	// Set up test context objects.
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	db := loopdb.NewStoreMock(t)
	addressStore := address.NewSqlStore(testDb.BaseDB)
	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.MainNetParams,
	)

	newID := func() deposit.ID {
		did, err := deposit.GetRandomDepositID()
		require.NoError(t, err)

		return did
	}
	_, pubClient := test.CreateKey(1)
	_, pubServer := test.CreateKey(2)
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
		AddressID: 1,
		AddressParams: &address.Parameters{
			ClientPubkey: pubClient,
			ServerPubkey: pubServer,
			PkScript:     []byte{0x00, 0x14, 0x1a, 0x2b, 0x3c},
			Expiry:       10_000,
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
			AddressID: 2,
			AddressParams: &address.Parameters{
				ClientPubkey: pubClient,
				ServerPubkey: pubServer,
				PkScript:     []byte{0x00, 0x14, 0x1a, 0x2b, 0x3d},
				Expiry:       20_000,
			},
		}

	err := addressStore.CreateStaticAddress(ctxb, d1.AddressParams)
	require.NoError(t, err)

	err = addressStore.CreateStaticAddress(ctxb, d2.AddressParams)
	require.NoError(t, err)

	err = depositStore.CreateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d2)
	require.NoError(t, err)

	outpoints := []string{
		d1.OutPoint.String(),
		d2.OutPoint.String(),
	}
	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	p2wkhAddr := "bcrt1qq68r6ff4k4pjx39efs44gcyccf7unqnu5qtjjz"
	addr, err := btcutil.DecodeAddress(p2wkhAddr, nil)
	require.NoError(t, err)

	swap := StaticAddressLoopIn{
		SwapHash:                lntypes.Hash{0x1, 0x2, 0x3, 0x4},
		DepositOutpoints:        outpoints,
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
		Deposits:                []*deposit.Deposit{d1, d2},
	}
	swap.SetState(Succeeded)

	err = swapStore.CreateLoopIn(ctxb, &swap)
	require.NoError(t, err)

	storedSwaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctxb, FinalStates,
	)
	require.NoError(t, err)
	require.EqualValues(t, 0, storedSwaps[0].SelectedAmount)

	err = MigrateSelectedSwapAmount(ctxb, db, depositStore, swapStore)
	require.NoError(t, err)

	storedSwaps, err = swapStore.GetStaticAddressLoopInSwapsByStates(
		ctxb, FinalStates,
	)
	require.NoError(t, err)
	require.EqualValues(t, d1.Value+d2.Value, storedSwaps[0].SelectedAmount)
}
