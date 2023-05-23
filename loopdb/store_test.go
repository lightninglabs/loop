package loopdb

import (
	"context"
	"crypto/sha256"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/coreos/bbolt"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	senderKey = [33]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2,
	}

	receiverKey = [33]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 3,
	}

	senderInternalKey = [33]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 4,
	}

	receiverInternalKey = [33]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 5,
	}

	testPreimage = lntypes.Preimage([32]byte{
		1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4,
		1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4,
	})

	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)
)

// TestLoopOutStore tests all the basic functionality of the current bbolt
// swap store.
func TestLoopOutStore(t *testing.T) {
	destAddr := test.GetDestAddr(t, 0)
	initiationTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)

	// Next, we'll make a new pending swap that we'll insert into the
	// database shortly.
	unrestrictedSwap := LoopOutContract{
		SwapContract: SwapContract{
			AmountRequested: 100,
			Preimage:        testPreimage,
			CltvExpiry:      144,
			HtlcKeys: HtlcKeys{
				SenderScriptKey:        senderKey,
				ReceiverScriptKey:      receiverKey,
				SenderInternalPubKey:   senderInternalKey,
				ReceiverInternalPubKey: receiverInternalKey,
				ClientScriptKeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  2,
				},
			},
			MaxMinerFee: 10,
			MaxSwapFee:  20,

			InitiationHeight: 99,

			// Convert to/from unix to remove timezone, so that it
			// doesn't interfere with DeepEqual.
			InitiationTime:  time.Unix(0, initiationTime.UnixNano()),
			ProtocolVersion: ProtocolVersionMuSig2,
		},
		MaxPrepayRoutingFee:     40,
		PrepayInvoice:           "prepayinvoice",
		DestAddr:                destAddr,
		SwapInvoice:             "swapinvoice",
		MaxSwapRoutingFee:       30,
		SweepConfTarget:         2,
		HtlcConfirmations:       2,
		SwapPublicationDeadline: time.Unix(0, initiationTime.UnixNano()),
	}

	t.Run("no outgoing set", func(t *testing.T) {
		testLoopOutStore(t, &unrestrictedSwap)
	})

	restrictedSwap := unrestrictedSwap
	restrictedSwap.OutgoingChanSet = ChannelSet{1, 2}

	t.Run("two channel outgoing set", func(t *testing.T) {
		testLoopOutStore(t, &restrictedSwap)
	})

	labelledSwap := unrestrictedSwap
	labelledSwap.Label = "test label"
	t.Run("labelled swap", func(t *testing.T) {
		testLoopOutStore(t, &labelledSwap)
	})
}

// testLoopOutStore tests the basic functionality of the current bbolt
// swap store for specific swap parameters.
func testLoopOutStore(t *testing.T, pendingSwap *LoopOutContract) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	require.NoError(t, err)

	defer os.RemoveAll(tempDirName)

	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	require.NoError(t, err)

	ctxb := context.Background()

	// First, verify that an empty database has no active swaps.
	swaps, err := store.FetchLoopOutSwaps(ctxb)

	require.NoError(t, err)
	require.Empty(t, swaps)

	hash := pendingSwap.Preimage.Hash()

	// checkSwap is a test helper function that'll assert the state of a
	// swap.
	checkSwap := func(expectedState SwapState) {
		t.Helper()

		swaps, err := store.FetchLoopOutSwaps(ctxb)
		require.NoError(t, err)

		require.Len(t, swaps, 1)

		swap, err := store.FetchLoopOutSwap(ctxb, hash)
		require.NoError(t, err)

		require.Equal(t, hash, swap.Hash)
		require.Equal(t, hash, swaps[0].Hash)

		swapContract := swap.Contract

		require.Equal(t, swapContract, pendingSwap)

		require.Equal(t, expectedState, swap.State().State)

		if expectedState == StatePreimageRevealed {
			require.NotNil(t, swap.State().HtlcTxHash)
		}
	}

	// If we create a new swap, then it should show up as being initialized
	// right after.
	err = store.CreateLoopOut(ctxb, hash, pendingSwap)
	require.NoError(t, err)

	checkSwap(StateInitiated)

	// Trying to make the same swap again should result in an error.
	err = store.CreateLoopOut(ctxb, hash, pendingSwap)
	require.Error(t, err)
	checkSwap(StateInitiated)

	// Next, we'll update to the next state of the pre-image being
	// revealed. The state should be reflected here again.
	err = store.UpdateLoopOut(
		ctxb, hash, testTime,
		SwapStateData{
			State:      StatePreimageRevealed,
			HtlcTxHash: &chainhash.Hash{1, 6, 2},
		},
	)
	require.NoError(t, err)

	checkSwap(StatePreimageRevealed)

	// Next, we'll update to the final state to ensure that the state is
	// properly updated.
	err = store.UpdateLoopOut(
		ctxb, hash, testTime,
		SwapStateData{
			State: StateFailInsufficientValue,
		},
	)
	require.NoError(t, err)
	checkSwap(StateFailInsufficientValue)

	err = store.Close()
	require.NoError(t, err)

	// If we re-open the same store, then the state of the current swap
	// should be the same.
	store, err = NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	require.NoError(t, err)

	checkSwap(StateFailInsufficientValue)
}

// TestLoopInStore tests all the basic functionality of the current bbolt
// swap store.
func TestLoopInStore(t *testing.T) {
	initiationTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)

	// Next, we'll make a new pending swap that we'll insert into the
	// database shortly.
	lastHop := route.Vertex{1, 2, 3}

	pendingSwap := LoopInContract{
		SwapContract: SwapContract{
			AmountRequested: 100,
			Preimage:        testPreimage,
			CltvExpiry:      144,
			HtlcKeys: HtlcKeys{
				SenderScriptKey:        senderKey,
				ReceiverScriptKey:      receiverKey,
				SenderInternalPubKey:   senderInternalKey,
				ReceiverInternalPubKey: receiverInternalKey,
				ClientScriptKeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  2,
				},
			},
			MaxMinerFee:      10,
			MaxSwapFee:       20,
			InitiationHeight: 99,

			// Convert to/from unix to remove timezone, so that it
			// doesn't interfere with DeepEqual.
			InitiationTime:  time.Unix(0, initiationTime.UnixNano()),
			ProtocolVersion: ProtocolVersionMuSig2,
		},
		HtlcConfTarget: 2,
		LastHop:        &lastHop,
		ExternalHtlc:   true,
	}

	t.Run("loop in", func(t *testing.T) {
		testLoopInStore(t, pendingSwap)
	})

	labelledSwap := pendingSwap
	labelledSwap.Label = "test label"
	t.Run("loop in with label", func(t *testing.T) {
		testLoopInStore(t, labelledSwap)
	})
}

func testLoopInStore(t *testing.T, pendingSwap LoopInContract) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	require.NoError(t, err)
	defer os.RemoveAll(tempDirName)

	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	require.NoError(t, err)

	ctxb := context.Background()

	// First, verify that an empty database has no active swaps.
	swaps, err := store.FetchLoopInSwaps(ctxb)
	require.NoError(t, err)
	require.Empty(t, swaps)

	hash := sha256.Sum256(testPreimage[:])

	// checkSwap is a test helper function that'll assert the state of a
	// swap.
	checkSwap := func(expectedState SwapState) {
		t.Helper()

		swaps, err := store.FetchLoopInSwaps(ctxb)
		require.NoError(t, err)
		require.Len(t, swaps, 1)

		swap := swaps[0].Contract

		require.Equal(t, swap, &pendingSwap)

		require.Equal(t, swaps[0].State().State, expectedState)
	}

	// If we create a new swap, then it should show up as being initialized
	// right after.
	err = store.CreateLoopIn(ctxb, hash, &pendingSwap)
	require.NoError(t, err)

	checkSwap(StateInitiated)

	// Trying to make the same swap again should result in an error.
	err = store.CreateLoopIn(ctxb, hash, &pendingSwap)
	require.Error(t, err)

	checkSwap(StateInitiated)

	// Next, we'll update to the next state of the pre-image being
	// revealed. The state should be reflected here again.
	err = store.UpdateLoopIn(
		ctxb, hash, testTime,
		SwapStateData{
			State: StatePreimageRevealed,
		},
	)
	require.NoError(t, err)

	checkSwap(StatePreimageRevealed)

	// Next, we'll update to the final state to ensure that the state is
	// properly updated.
	err = store.UpdateLoopIn(
		ctxb, hash, testTime,
		SwapStateData{
			State: StateFailInsufficientValue,
		},
	)
	require.NoError(t, err)
	checkSwap(StateFailInsufficientValue)

	err = store.Close()
	require.NoError(t, err)

	// If we re-open the same store, then the state of the current swap
	// should be the same.
	store, err = NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	require.NoError(t, err)

	checkSwap(StateFailInsufficientValue)
}

// TestVersionNew tests that a new database is initialized with the current
// version.
func TestVersionNew(t *testing.T) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDirName)

	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}

	ver, err := getDBVersion(store.db)
	if err != nil {
		t.Fatal(err)
	}

	if ver != latestDBVersion {
		t.Fatal("db not at latest version")
	}
}

// TestVersionNew tests that an existing version zero database is migrated to
// the latest version.
func TestVersionMigrated(t *testing.T) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDirName)

	createVersionZeroDb(t, tempDirName)

	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}

	ver, err := getDBVersion(store.db)
	if err != nil {
		t.Fatal(err)
	}

	if ver != latestDBVersion {
		t.Fatal("db not at latest version")
	}
}

// createVersionZeroDb creates a database with an empty meta bucket. In version
// zero, there was no version key specified yet.
func createVersionZeroDb(t *testing.T, dbPath string) {
	path := filepath.Join(dbPath, dbFileName)
	bdb, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()

	err = bdb.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucket(metaBucketKey)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLegacyOutgoingChannel asserts that a legacy channel restriction is
// properly mapped onto the newer channel set.
func TestLegacyOutgoingChannel(t *testing.T) {
	var (
		legacyDbVersion       = Hex("00000003")
		legacyOutgoingChannel = Hex("0000000000000005")
	)

	ctxb := context.Background()

	legacyDb := map[string]interface{}{
		"loop-in": map[string]interface{}{},
		"metadata": map[string]interface{}{
			"dbp": legacyDbVersion,
		},
		"uncharge-swaps": map[string]interface{}{
			Hex("2a595d79a55168970532805ae20c9b5fac98f04db79ba4c6ae9b9ac0f206359e"): map[string]interface{}{
				"contract": Hex("1562d6fbec140000010101010202020203030303040404040101010102020202030303030404040400000000000000640d707265706179696e766f69636501010101010101010101010101010101010101010101010101010101010101010201010101010101010101010101010101010101010101010101010101010101010300000090000000000000000a0000000000000014000000000000002800000063223347454e556d6e4552745766516374344e65676f6d557171745a757a5947507742530b73776170696e766f69636500000002000000000000001e") + legacyOutgoingChannel + Hex("1562d6fbec140000"),
				"updates": map[string]interface{}{
					Hex("0000000000000001"): Hex("1508290a92d4c00001000000000000000000000000000000000000000000000000"),
					Hex("0000000000000002"): Hex("1508290a92d4c00006000000000000000000000000000000000000000000000000"),
				},
			},
		},
	}

	// Restore a legacy database.
	tempDirName, err := ioutil.TempDir("", "clientstore")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDirName)

	tempPath := filepath.Join(tempDirName, dbFileName)
	db, err := bbolt.Open(tempPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		return RestoreDB(tx, legacyDb)
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Fetch the legacy swap.
	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}

	swaps, err := store.FetchLoopOutSwaps(ctxb)
	if err != nil {
		t.Fatal(err)
	}

	// Assert that the outgoing channel is read properly.
	expectedChannelSet := ChannelSet{5}

	require.Equal(t, expectedChannelSet, swaps[0].Contract.OutgoingChanSet)
}

// TestLiquidityParams checks that reading and writing to liquidty bucket are
// as expected.
func TestLiquidityParams(t *testing.T) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	require.NoError(t, err, "failed to db")
	defer os.RemoveAll(tempDirName)

	ctxb := context.Background()

	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	require.NoError(t, err, "failed to create store")

	// Test when there's no params saved before, an empty bytes is
	// returned.
	params, err := store.FetchLiquidityParams(ctxb)
	require.NoError(t, err, "failed to fetch params")
	require.Empty(t, params, "expect empty bytes")
	require.Nil(t, params)

	params = []byte("test")

	// Test we can save the params.
	err = store.PutLiquidityParams(ctxb, params)
	require.NoError(t, err, "failed to put params")

	// Now fetch the db again should return the above saved bytes.
	paramsRead, err := store.FetchLiquidityParams(ctxb)
	require.NoError(t, err, "failed to fetch params")
	require.Equal(t, params, paramsRead, "unexpected return value")
}
