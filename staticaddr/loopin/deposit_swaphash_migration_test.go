package loopin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	loop_test "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

const (
	P2wkhAddr = "bcrt1qq68r6ff4k4pjx39efs44gcyccf7unqnu5qtjjz"
)

// TestDepositSwapHashMigration tests deposit to swap hash migration.
func TestDepositSwapHashMigration(t *testing.T) {
	// Set up test context objects.
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	db := loopdb.NewStoreMock(t)
	testClock := clock.NewTestClock(time.Now())
	defer testDb.Close()

	addressStore := address.NewSqlStore(testDb.BaseDB)
	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb), testClock,
		&chaincfg.RegressionNetParams,
	)

	_, client := loop_test.CreateKey(1)
	_, server := loop_test.CreateKey(2)
	pkScript := []byte("pkscript")
	addrParams := &address.Parameters{
		ClientPubkey: client,
		ServerPubkey: server,
		Expiry:       10,
		PkScript:     pkScript,
	}

	err := addressStore.CreateStaticAddress(t.Context(), addrParams)
	require.NoError(t, err)
	addrParams.PkScript = []byte("pkscript2")
	err = addressStore.CreateStaticAddress(t.Context(), addrParams)
	require.NoError(t, err)

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
		AddressID: 1,
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
		}

	err = depositStore.CreateDeposit(ctxb, d1)
	require.NoError(t, err)
	err = depositStore.CreateDeposit(ctxb, d2)
	require.NoError(t, err)

	outpoints := []string{
		d1.OutPoint.String(),
		d2.OutPoint.String(),
	}
	_, clientPubKey := loop_test.CreateKey(1)
	_, serverPubKey := loop_test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	swapHash := lntypes.Hash{0x1, 0x2, 0x3, 0x4}
	loopIn := StaticAddressLoopIn{
		SwapHash:                swapHash,
		DepositOutpoints:        outpoints,
		ClientPubkey:            clientPubKey,
		ServerPubkey:            serverPubKey,
		HtlcTimeoutSweepAddress: addr,
	}
	loopIn.SetState(Succeeded)

	// Insert the swap without the deposit mapping.
	err = swapStore.baseDB.ExecTx(ctxb, loopdb.NewSqlWriteOpts(),
		func(q Querier) error {
			swapArgs := sqlc.InsertSwapParams{
				SwapHash:         loopIn.SwapHash[:],
				Preimage:         loopIn.SwapPreimage[:],
				InitiationTime:   loopIn.InitiationTime,
				AmountRequested:  int64(loopIn.TotalDepositAmount()),
				CltvExpiry:       loopIn.HtlcCltvExpiry,
				MaxSwapFee:       int64(loopIn.MaxSwapFee),
				InitiationHeight: int32(loopIn.InitiationHeight),
				ProtocolVersion:  int32(loopIn.ProtocolVersion),
				Label:            loopIn.Label,
			}

			htlcKeyArgs := sqlc.InsertHtlcKeysParams{
				SwapHash:             loopIn.SwapHash[:],
				SenderScriptPubkey:   loopIn.ClientPubkey.SerializeCompressed(),
				ReceiverScriptPubkey: loopIn.ServerPubkey.SerializeCompressed(),
				ClientKeyFamily:      int32(loopIn.HtlcKeyLocator.Family),
				ClientKeyIndex:       int32(loopIn.HtlcKeyLocator.Index),
			}

			// Sanity check, if any of the outpoints contain the
			// outpoint separator. If so, we reject the loop-in to
			// prevent potential issues with parsing.
			for _, outpoint := range loopIn.DepositOutpoints {
				if strings.Contains(outpoint, OutpointSeparator) {
					return ErrInvalidOutpoint
				}
			}

			joinedOutpoints := strings.Join(
				loopIn.DepositOutpoints, OutpointSeparator,
			)
			staticAddressLoopInParams := sqlc.InsertStaticAddressLoopInParams{
				SwapHash:                loopIn.SwapHash[:],
				SwapInvoice:             loopIn.SwapInvoice,
				LastHop:                 loopIn.LastHop,
				QuotedSwapFeeSatoshis:   int64(loopIn.QuotedSwapFee),
				HtlcTimeoutSweepAddress: loopIn.HtlcTimeoutSweepAddress.String(),
				HtlcTxFeeRateSatKw:      int64(loopIn.HtlcTxFeeRate),
				DepositOutpoints:        joinedOutpoints,
				PaymentTimeoutSeconds:   int32(loopIn.PaymentTimeoutSeconds),
			}

			updateArgs := sqlc.InsertStaticAddressMetaUpdateParams{
				SwapHash:        loopIn.SwapHash[:],
				UpdateTimestamp: testClock.Now(),
				UpdateState:     string(loopIn.GetState()),
			}
			err := q.InsertSwap(ctxb, swapArgs)
			if err != nil {
				return err
			}

			err = q.InsertHtlcKeys(ctxb, htlcKeyArgs)
			if err != nil {
				return err
			}

			err = q.InsertStaticAddressLoopIn(
				ctxb, staticAddressLoopInParams,
			)
			if err != nil {
				return err
			}

			return q.InsertStaticAddressMetaUpdate(ctxb, updateArgs)
		},
	)
	require.NoError(t, err)

	depositIDs, err := swapStore.DepositIDsForSwapHash(ctxb, swapHash)
	require.NoError(t, err)
	require.Len(t, depositIDs, 0)

	swapHashes, err := swapStore.SwapHashesForDepositIDs(
		ctxb, []deposit.ID{d1.ID, d2.ID},
	)
	require.NoError(t, err)
	require.Len(t, swapHashes, 0)

	err = MigrateDepositSwapHash(ctxb, db, depositStore, swapStore)
	require.NoError(t, err)

	depositIDs, err = swapStore.DepositIDsForSwapHash(ctxb, swapHash)
	require.NoError(t, err)
	require.Len(t, depositIDs, 2)
	require.Contains(t, depositIDs, d1.ID)
	require.Contains(t, depositIDs, d2.ID)

	swapHashes, err = swapStore.SwapHashesForDepositIDs(
		ctxb, []deposit.ID{d1.ID, d2.ID},
	)
	require.NoError(t, err)
	require.Len(t, swapHashes, 1)
	require.Len(t, swapHashes[swapHash], 2)
	require.Contains(t, swapHashes[swapHash], d1.ID)
	require.Contains(t, swapHashes[swapHash], d2.ID)
}
