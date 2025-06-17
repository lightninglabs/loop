package assets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	defaultClientPubkeyBytes, _ = hex.DecodeString("021c97a90a411ff2b10dc2a8e32de2f29d2fa49d41bfbb52bd416e460db0747d0d")
	defaultClientPubkey, _      = btcec.ParsePubKey(defaultClientPubkeyBytes)

	defaultOutpoint = &wire.OutPoint{
		Hash:  chainhash.Hash{0x01},
		Index: 1,
	}
)

// TestSqlStore tests the asset swap store.
func TestSqlStore(t *testing.T) {
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	store := NewPostgresStore(testDb)

	swapPreimage := getRandomPreimage()
	SwapHash := swapPreimage.Hash()

	// Create a new SwapOut.
	swapOut := &SwapOut{
		SwapKit: htlc.SwapKit{
			SwapHash:       SwapHash,
			Amount:         100,
			SenderPubKey:   defaultClientPubkey,
			ReceiverPubKey: defaultClientPubkey,
			CsvExpiry:      100,
			AssetID:        []byte("assetid"),
		},
		SwapPreimage:     swapPreimage,
		State:            fsm.StateType("init"),
		InitiationHeight: 1,
		ClientKeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  1,
		},
	}

	// Save the swap out in the db.
	err := store.CreateAssetSwapOut(ctxb, swapOut)
	require.NoError(t, err)

	// Insert a new swap out update.
	err = store.InsertAssetSwapUpdate(
		ctxb, SwapHash, fsm.StateType("state2"),
	)
	require.NoError(t, err)

	// Try to fetch all swap outs.
	swapOuts, err := store.GetAllAssetOuts(ctxb)
	require.NoError(t, err)
	require.Len(t, swapOuts, 1)

	// Update the htlc outpoint.
	err = store.UpdateAssetSwapHtlcOutpoint(
		ctxb, SwapHash, defaultOutpoint, 100,
	)
	require.NoError(t, err)

	// Update the offchain payment amount.
	err = store.UpdateAssetSwapOutProof(
		ctxb, SwapHash, []byte("proof"),
	)
	require.NoError(t, err)

	// Try to fetch all active swap outs.
	activeSwapOuts, err := store.GetActiveAssetOuts(ctxb)
	require.NoError(t, err)
	require.Len(t, activeSwapOuts, 1)

	// TODO: Uncomment this when we have a way to get the finished
	// states from the database.
	// // Update the swap out state to a finished state.
	// err = store.InsertAssetSwapUpdate(
	// 	ctxb, SwapHash, fsm.StateType(FinishedStates()[0]),
	// )
	// require.NoError(t, err)

	// // Try to fetch all active swap outs.
	// activeSwapOuts, err = store.GetActiveAssetOuts(ctxb)
	// require.NoError(t, err)
	// require.Len(t, activeSwapOuts, 0)
}

// getRandomPreimage generates a random reservation ID.
func getRandomPreimage() lntypes.Preimage {
	var id lntypes.Preimage
	_, err := rand.Read(id[:])
	if err != nil {
		panic(err)
	}
	return id
}
