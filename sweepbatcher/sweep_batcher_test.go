package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	swapInvoice = "lntb1230n1pjjszzgpp5j76f03wrkya4sm4gxv6az5nmz5aqsvmn4" +
		"tpguu2sdvdyygedqjgqdq9xyerxcqzzsxqr23ssp5rwzmwtfjmsgranfk8sr" +
		"4p4gcgmvyd42uug8pxteg2mkk23ndvkqs9qyyssq44ruk3ex59cmv4dm6k4v" +
		"0kc6c0gcqjs0gkljfyd6c6uatqa2f67xlx3pcg5tnvcae5p3jju8ra77e87d" +
		"vhhs0jrx53wnc0fq9rkrhmqqelyx7l"

	eventuallyCheckFrequency = 100 * time.Millisecond

	ntfnBufferSize = 1024
)

// destAddr is a dummy p2wkh address to use as the destination address for
// the swaps.
var destAddr = func() btcutil.Address {
	p2wkhAddr := "bcrt1qq68r6ff4k4pjx39efs44gcyccf7unqnu5qtjjz"
	addr, err := btcutil.DecodeAddress(p2wkhAddr, nil)
	if err != nil {
		panic(err)
	}
	return addr
}()

var htlcKeys = func() loopdb.HtlcKeys {
	var senderKey, receiverKey [33]byte

	// Generate keys.
	_, senderPubKey := test.CreateKey(1)
	copy(senderKey[:], senderPubKey.SerializeCompressed())
	_, receiverPubKey := test.CreateKey(2)
	copy(receiverKey[:], receiverPubKey.SerializeCompressed())

	return loopdb.HtlcKeys{
		SenderScriptKey:        senderKey,
		ReceiverScriptKey:      receiverKey,
		SenderInternalPubKey:   senderKey,
		ReceiverInternalPubKey: receiverKey,
	}
}()

func testVerifySchnorrSig(pubKey *btcec.PublicKey, hash, sig []byte) error {
	return nil
}

func testMuSig2SignSweep(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
	prevoutMap map[wire.OutPoint]*wire.TxOut) (
	[]byte, []byte, error) {

	return nil, nil, nil
}

var customSignature = func() []byte {
	sig := [64]byte{10, 20, 30}
	return sig[:]
}()

func testSignMuSig2func(ctx context.Context, muSig2Version input.MuSig2Version,
	swapHash lntypes.Hash, rootHash chainhash.Hash,
	sigHash [32]byte) ([]byte, error) {

	return customSignature, nil
}

var dummyNotifier = SpendNotifier{
	SpendChan:    make(chan *SpendDetail, ntfnBufferSize),
	SpendErrChan: make(chan error, ntfnBufferSize),
	QuitChan:     make(chan bool, ntfnBufferSize),
}

func checkBatcherError(t *testing.T, err error) {
	if !errors.Is(err, context.Canceled) &&
		!errors.Is(err, ErrBatcherShuttingDown) &&
		!errors.Is(err, ErrBatchShuttingDown) {

		require.NoError(t, err)
	}
}

// getOnlyBatch makes sure the batcher has exactly one batch and returns it.
func getOnlyBatch(batcher *Batcher) *batch {
	if len(batcher.batches) != 1 {
		panic(fmt.Sprintf("getOnlyBatch called on a batcher having "+
			"%d batches", len(batcher.batches)))
	}

	for _, batch := range batcher.batches {
		return batch
	}

	panic("unreachable")
}

// testSweepBatcherBatchCreation tests that sweep requests enter the expected
// batch based on their timeout distance.
func testSweepBatcherBatchCreation(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create a sweep request.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Insert the same swap twice, this should be a noop.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Create a second sweep request that has a timeout distance less than
	// our configured threshold.
	sweepReq2 := SweepRequest{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 2,
		},
		Notifier: &dummyNotifier,
	}

	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance - 1,
			AmountRequested: 222,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Tick tock next block.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Batcher should not create a second batch as timeout distance is small
	// enough.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Create a third sweep request that has more timeout distance than
	// the default.
	sweepReq3 := SweepRequest{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    333,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{3, 3},
			Index: 3,
		},
		Notifier: &dummyNotifier,
	}

	swap3 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance + 1,
			AmountRequested: 333,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{3},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Batcher should create a second batch as timeout distance is greater
	// than the threshold
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since the second batch got created we check that it registered its
	// primary sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps
		// in it.
		for _, batch := range batcher.batches {
			switch batch.primarySweepID {
			case sweepReq1.SwapHash:
				if len(batch.sweeps) != 2 {
					return false
				}

			case sweepReq3.SwapHash:
				if len(batch.sweeps) != 1 {
					return false
				}
			}
		}

		return true
	}, test.Timeout, eventuallyCheckFrequency)

	// Check that all sweeps were stored.
	require.True(t, batcherStore.AssertSweepStored(sweepReq1.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq2.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq3.SwapHash))
}

// testFeeBumping tests that sweep is RBFed with slightly higher fee rate after
// each block unless WithCustomFeeRate is passed.
func testFeeBumping(t *testing.T, store testStore,
	batcherStore testBatcherStore, noFeeBumping bool) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	// Disable fee bumping, if requested.
	var opts []BatcherOption
	if noFeeBumping {
		customFeeRate := func(ctx context.Context,
			swapHash lntypes.Hash) (chainfee.SatPerKWeight, error) {

			// Always provide the same value, no bumping.
			return test.DefaultMockFee, nil
		}

		opts = append(opts, WithCustomFeeRate(customFeeRate))
	}

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, opts...)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create a sweep request.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    1_000_000,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 1_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	tx1 := <-lnd.TxPublishChannel
	out1 := tx1.TxOut[0].Value

	// Tick tock next block.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for another sweep tx to be published.
	tx2 := <-lnd.TxPublishChannel
	out2 := tx2.TxOut[0].Value

	if noFeeBumping {
		// Expect output to stay the same.
		require.Equal(t, out1, out2, "expected out to stay the same")
	} else {
		// Expect output to drop.
		require.Greater(t, out1, out2, "expected out to drop")
	}
}

// testSweepBatcherSimpleLifecycle tests the simple lifecycle of the batches
// that are created and run by the batcher.
func testSweepBatcherSimpleLifecycle(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create a sweep request.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Eventually request will be consumed and a new batch will spin up.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// When batch is successfully created it will execute it's first step,
	// which leads to a spend monitor of the primary sweep.
	<-lnd.RegisterSpendChannel

	// Find the batch and assign it to a local variable for easier access.
	batch := &batch{}
	for _, btch := range batcher.batches {
		if btch.primarySweepID == sweepReq1.SwapHash {
			batch = btch
		}
	}

	require.Eventually(t, func() bool {
		// Batch should have the sweep stored.
		return len(batch.sweeps) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// The primary sweep id should be that of the first inserted sweep.
	require.Equal(t, batch.primarySweepID, sweepReq1.SwapHash)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// After receiving a height notification the batch will step again,
	// leading to a new spend monitoring.
	require.Eventually(t, func() bool {
		return batch.currentHeight == 601
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Create the spending tx that will trigger the spend monitor of the
	// batch.
	spendingTx := &wire.MsgTx{
		Version: 1,
		// Since the spend monitor is registered on the primary sweep's
		// outpoint we insert that outpoint here.
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: sweepReq1.Outpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				PkScript: []byte{3, 2, 1},
			},
		},
	}

	spendingTxHash := spendingTx.TxHash()

	// Mock the spend notification that spends the swap.
	spendDetail := &chainntnfs.SpendDetail{
		SpentOutPoint:     &sweepReq1.Outpoint,
		SpendingTx:        spendingTx,
		SpenderTxHash:     &spendingTxHash,
		SpenderInputIndex: 0,
		SpendingHeight:    601,
	}

	// We notify the spend.
	lnd.SpendChannel <- spendDetail

	// After receiving the spend, the batch is now monitoring for confs.
	<-lnd.RegisterConfChannel

	// The batch should eventually read the spend notification and progress
	// its state to closed.
	require.Eventually(t, func() bool {
		return batch.state == Closed
	}, test.Timeout, eventuallyCheckFrequency)

	err = lnd.NotifyHeight(604)
	require.NoError(t, err)

	// We mock the tx confirmation notification.
	lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: spendingTx,
	}

	// Eventually the batch receives the confirmation notification and
	// confirms itself.
	require.Eventually(t, func() bool {
		return batch.isComplete()
	}, test.Timeout, eventuallyCheckFrequency)
}

// testSweepBatcherSweepReentry tests that when an old version of the batch tx
// gets confirmed the sweep leftovers are sent back to the batcher.
func testSweepBatcherSweepReentry(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create some sweep requests with timeouts not too far away, in order
	// to enter the same batch.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	sweepReq2 := SweepRequest{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 2,
		},
		Notifier: &dummyNotifier,
	}

	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 222,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	sweepReq3 := SweepRequest{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    333,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{3, 3},
			Index: 3,
		},
		Notifier: &dummyNotifier,
	}

	swap3 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 333,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{3},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Feed the sweeps to the batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// After inserting the primary (first) sweep, a spend monitor should be
	// registered.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Add the second sweep.
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Add next block to trigger batch publishing.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Add the third sweep.
	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Add next block to trigger batch publishing.
	err = lnd.NotifyHeight(602)
	require.NoError(t, err)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Batcher should create a batch for the sweeps.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Find the batch and store it in a local variable for easier access.
	b := &batch{}
	for _, btch := range batcher.batches {
		if btch.primarySweepID == sweepReq1.SwapHash {
			b = btch
		}
	}

	// Batcher should contain all sweeps.
	require.Eventually(t, func() bool {
		return len(b.sweeps) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	// Verify that the batch has a primary sweep id that matches the first
	// inserted sweep, sweep1.
	require.Equal(t, b.primarySweepID, sweepReq1.SwapHash)

	// Create the spending tx. In order to simulate an older version of the
	// batch transaction being confirmed, we only insert the primary sweep's
	// outpoint as a TxIn. This means that the other two sweeps did not
	// appear in the spending transaction. (This simulates a possible
	// scenario caused by RBF replacements.)
	spendingTx := &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: sweepReq1.Outpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: int64(sweepReq1.Value.ToUnit(
					btcutil.AmountSatoshi,
				)),
				PkScript: []byte{3, 2, 1},
			},
		},
	}

	spendingTxHash := spendingTx.TxHash()

	spendDetail := &chainntnfs.SpendDetail{
		SpentOutPoint:     &sweepReq1.Outpoint,
		SpendingTx:        spendingTx,
		SpenderTxHash:     &spendingTxHash,
		SpenderInputIndex: 0,
		SpendingHeight:    603,
	}

	// Send the spending notification to the mock channel.
	lnd.SpendChannel <- spendDetail

	// After receiving the spend notification the batch should progress to
	// the next step, which is monitoring for confirmations.
	<-lnd.RegisterConfChannel

	// Eventually the batch reads the notification and proceeds to a closed
	// state.
	require.Eventually(t, func() bool {
		return b.state == Closed
	}, test.Timeout, eventuallyCheckFrequency)

	// While handling the spend notification the batch should detect that
	// some sweeps did not appear in the spending tx, therefore it redirects
	// them back to the batcher and the batcher inserts them in a new batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since second batch was created we check that it registered for its
	// primary sweep's spend.
	<-lnd.RegisterSpendChannel

	// We mock the confirmation notification.
	lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: spendingTx,
	}

	// Wait for tx to be published.
	// Here is a race condition, which is unlikely to cause a crash: if we
	// wait for publish tx before sending a conf notification (previous
	// action), then conf notification can go to the second batch (since
	// the mock does not have a way to direct a notification to proper
	// subscriber) and the first batch does not exit, waiting for the
	// confirmation forever.
	<-lnd.TxPublishChannel

	// Eventually the batch receives the confirmation notification,
	// gracefully exits and the batcher deletes it.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Find the other batch, which includes the sweeps that did not appear
	// in the spending tx.
	b = &batch{}
	for _, btch := range batcher.batches {
		b = btch
	}

	// After all the sweeps enter, it should contain 2 sweeps.
	require.Eventually(t, func() bool {
		return len(b.sweeps) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// The batch should be in an open state.
	require.Equal(t, b.state, Open)
}

// testSweepBatcherNonWalletAddr tests that sweep requests that sweep to a non
// wallet address enter individual batches.
func testSweepBatcherNonWalletAddr(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create a sweep request.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},
		IsExternalAddr:  true,
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Insert the same swap twice, this should be a noop.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Create a second sweep request that has a timeout distance less than
	// our configured threshold.
	sweepReq2 := SweepRequest{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 2,
		},
		Notifier: &dummyNotifier,
	}

	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance - 1,
			AmountRequested: 222,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},
		IsExternalAddr:  true,
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Batcher should create a second batch as first batch is a non wallet
	// addr batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for second batch to be published.
	<-lnd.TxPublishChannel

	// Create a third sweep request that has more timeout distance than
	// the default.
	sweepReq3 := SweepRequest{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    333,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{3, 3},
			Index: 3,
		},
		Notifier: &dummyNotifier,
	}

	swap3 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance + 1,
			AmountRequested: 222,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{3},
		},
		IsExternalAddr:  true,
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Batcher should create a new batch as timeout distance is greater than
	// the threshold
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published for 3rd batch.
	<-lnd.TxPublishChannel

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps
		// in it.
		for _, batch := range batcher.batches {
			switch batch.primarySweepID {
			case sweepReq1.SwapHash:
				if len(batch.sweeps) != 1 {
					return false
				}

			case sweepReq2.SwapHash:
				if len(batch.sweeps) != 1 {
					return false
				}

			case sweepReq3.SwapHash:
				if len(batch.sweeps) != 1 {
					return false
				}
			}
		}

		return true
	}, test.Timeout, eventuallyCheckFrequency)

	// Check that all sweeps were stored.
	require.True(t, batcherStore.AssertSweepStored(sweepReq1.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq2.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq3.SwapHash))
}

// testSweepBatcherComposite tests that sweep requests that sweep to both wallet
// addresses and non-wallet addresses enter the correct batches.
func testSweepBatcherComposite(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create a sweep request.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Create a second sweep request that has a timeout distance less than
	// our configured threshold.
	sweepReq2 := SweepRequest{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 2,
		},
		Notifier: &dummyNotifier,
	}

	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance - 1,
			AmountRequested: 222,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Create a third sweep request that has less timeout distance than the
	// default max, but is not spending to a wallet address.
	sweepReq3 := SweepRequest{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    333,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{3, 3},
			Index: 3,
		},
		Notifier: &dummyNotifier,
	}

	swap3 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance - 3,
			AmountRequested: 333,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{3},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
		IsExternalAddr:  true,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Create a fourth sweep request that has a timeout which is not valid
	// for the first batch, so it will cause it to create a new batch.
	sweepReq4 := SweepRequest{
		SwapHash: lntypes.Hash{4, 4, 4},
		Value:    444,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{4, 4},
			Index: 4,
		},
		Notifier: &dummyNotifier,
	}

	swap4 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance + 1,
			AmountRequested: 444,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{4},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq4.SwapHash, swap4)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Create a fifth sweep request that has a timeout which is not valid
	// for the first batch, but a valid timeout for the new batch.
	sweepReq5 := SweepRequest{
		SwapHash: lntypes.Hash{5, 5, 5},
		Value:    555,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{5, 5},
			Index: 5,
		},
		Notifier: &dummyNotifier,
	}

	swap5 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance + 5,
			AmountRequested: 555,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{5},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq5.SwapHash, swap5)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Create a sixth sweep request that has a valid timeout for the new
	// batch, but is paying to a non-wallet address.
	sweepReq6 := SweepRequest{
		SwapHash: lntypes.Hash{6, 6, 6},
		Value:    666,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{6, 6},
			Index: 6,
		},
		Notifier: &dummyNotifier,
	}

	swap6 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111 + defaultMaxTimeoutDistance + 6,
			AmountRequested: 666,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{6},
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
		IsExternalAddr:  true,
	}

	err = store.CreateLoopOut(ctx, sweepReq6.SwapHash, swap6)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Insert the same swap twice, this should be a noop.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Batcher should not create a second batch as timeout distance is small
	// enough.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Publish a block to trigger batch 1 republishing.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for tx for the first batch to be published (2 sweeps).
	tx := <-lnd.TxPublishChannel
	require.Equal(t, 2, len(tx.TxIn))

	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Batcher should create a second batch as this sweep pays to a non
	// wallet address.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx for the second batch to be published (1 sweep).
	tx = <-lnd.TxPublishChannel
	require.Equal(t, 1, len(tx.TxIn))

	require.NoError(t, batcher.AddSweep(&sweepReq4))

	// Batcher should create a third batch as timeout distance is greater
	// than the threshold.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx for the third batch to be published (1 sweep).
	tx = <-lnd.TxPublishChannel
	require.Equal(t, 1, len(tx.TxIn))

	require.NoError(t, batcher.AddSweep(&sweepReq5))

	// Publish a block to trigger batch 3 republishing.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for 3 txs for the 3 batches.
	<-lnd.TxPublishChannel
	<-lnd.TxPublishChannel
	<-lnd.TxPublishChannel

	// Batcher should not create a fourth batch as timeout distance is small
	// enough for it to join the last batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	require.NoError(t, batcher.AddSweep(&sweepReq6))

	// Batcher should create a fourth batch as this sweep pays to a non
	// wallet address.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 4
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx for the 4th batch to be published (1 sweep).
	tx = <-lnd.TxPublishChannel
	require.Equal(t, 1, len(tx.TxIn))

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps in
		// it.
		for _, batch := range batcher.batches {
			switch batch.primarySweepID {
			case sweepReq1.SwapHash:
				if len(batch.sweeps) != 2 {
					return false
				}

			case sweepReq3.SwapHash:
				if len(batch.sweeps) != 1 {
					return false
				}

			case sweepReq4.SwapHash:
				if len(batch.sweeps) != 2 {
					return false
				}

			case sweepReq6.SwapHash:
				if len(batch.sweeps) != 1 {
					return false
				}
			}
		}

		return true
	}, test.Timeout, eventuallyCheckFrequency)

	// Check that all sweeps were stored.
	require.True(t, batcherStore.AssertSweepStored(sweepReq1.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq2.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq3.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq4.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq5.SwapHash))
	require.True(t, batcherStore.AssertSweepStored(sweepReq6.SwapHash))
}

// makeTestTx creates a test transaction with a single output of the given
// value.
func makeTestTx(value int64) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(wire.NewTxOut(value, nil))
	return tx
}

// testGetFeePortionForSweep tests that the fee portion for a sweep is correctly
// calculated.
func testGetFeePortionForSweep(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	tests := []struct {
		name                 string
		spendTxValue         int64
		numSweeps            int
		totalSweptAmt        btcutil.Amount
		expectedFeePortion   btcutil.Amount
		expectedRoundingDiff btcutil.Amount
	}{
		{
			"Even Split",
			100, 5, 200, 20, 0,
		},
		{
			"Single Sweep",
			100, 1, 200, 100, 0,
		},
		{
			"With Rounding Diff",
			200, 4, 350, 37, 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spendTx := makeTestTx(tt.spendTxValue)
			feePortion, roundingDiff := getFeePortionForSweep(
				spendTx, tt.numSweeps, tt.totalSweptAmt,
			)
			require.Equal(t, tt.expectedFeePortion, feePortion)
			require.Equal(t, tt.expectedRoundingDiff, roundingDiff)
		})
	}
}

// testRestoringEmptyBatch tests that the batcher can be restored with an empty
// batch.
func testRestoringEmptyBatch(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	_, err = batcherStore.InsertSweepBatch(ctx, &dbBatch{})
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Create a sweep request.
	sweepReq := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq.SwapHash, swap)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored and we have exactly one
		// active batch.
		return batcherStore.AssertSweepStored(sweepReq.SwapHash) &&
			len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure we have only one batch stored (as we dropped the dormant
	// one).
	batches, err := batcherStore.FetchUnconfirmedSweepBatches(ctx)
	require.NoError(t, err)
	require.Len(t, batches, 1)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	checkBatcherError(t, runErr)
}

type loopStoreMock struct {
	loops map[lntypes.Hash]*loopdb.LoopOut
	mu    sync.Mutex

	// backend is the store passed to the test. An empty swap with the ID
	// passed is stored to this place to satisfy SQL foreign key constraint.
	backend testStore

	// preimage is last preimage first byte used in fake swap in backend.
	// It has to be unique to satisfy SQL constraint.
	preimage byte
}

func newLoopStoreMock(backend testStore) *loopStoreMock {
	return &loopStoreMock{
		loops:   make(map[lntypes.Hash]*loopdb.LoopOut),
		backend: backend,
	}
}

func (s *loopStoreMock) FetchLoopOutSwap(ctx context.Context,
	hash lntypes.Hash) (*loopdb.LoopOut, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	out, has := s.loops[hash]
	if !has {
		return nil, errors.New("loop not found")
	}

	return out, nil
}

func (s *loopStoreMock) putLoopOutSwap(hash lntypes.Hash, out *loopdb.LoopOut) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, existed := s.loops[hash]
	s.loops[hash] = out

	if existed {
		// The swap exists, no need to create one in backend, since it
		// stores fake data anyway.
		return
	}

	if _, ok := s.backend.(*loopdb.StoreMock); ok {
		// Do not create a fake loop in loopdb.StoreMock, because it
		// blocks on notification channels and this is not needed.
		return
	}

	// Put a swap with the same ID to backend store to satisfy SQL foreign
	// key constraint. Don't store the data to ensure it is not used.
	err := s.backend.CreateLoopOut(context.Background(), hash,
		&loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      999,
				AmountRequested: 999,

				// Make preimage unique to pass SQL constraints.
				Preimage: lntypes.Preimage{s.preimage},
			},

			DestAddr:    destAddr,
			SwapInvoice: swapInvoice,
		},
	)

	s.backend.AssertLoopOutStored()

	// Make preimage unique to pass SQL constraints.
	s.preimage++

	if err != nil {
		panic(err)
	}
}

// AssertLoopOutStored asserts that a swap is stored.
func (s *loopStoreMock) AssertLoopOutStored() {
	s.backend.AssertLoopOutStored()
}

// testHandleSweepTwice tests that handing the same sweep twice must not
// add it to different batches.
func testHandleSweepTwice(t *testing.T, backend testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	store := newLoopStoreMock(backend)
	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	const shortCltv = 111
	const longCltv = 111 + defaultMaxTimeoutDistance + 6

	// Create two sweep requests with CltvExpiry distant from each other
	// to go assigned to separate batches.
	sweepReq1 := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	loopOut1 := &loopdb.LoopOut{
		Loop: loopdb.Loop{
			Hash: lntypes.Hash{1, 1, 1},
		},
		Contract: &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      shortCltv,
				AmountRequested: 111,
				ProtocolVersion: loopdb.ProtocolVersionMuSig2,
				HtlcKeys:        htlcKeys,
			},
			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 111,
		},
	}

	sweepReq2 := SweepRequest{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 2,
		},
		Notifier: &dummyNotifier,
	}

	loopOut2 := &loopdb.LoopOut{
		Loop: loopdb.Loop{
			Hash: lntypes.Hash{2, 2, 2},
		},
		Contract: &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      longCltv,
				AmountRequested: 222,
				ProtocolVersion: loopdb.ProtocolVersionMuSig2,
				HtlcKeys:        htlcKeys,
			},
			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 111,
		},
	}

	store.putLoopOutSwap(sweepReq1.SwapHash, loopOut1)
	store.putLoopOutSwap(sweepReq2.SwapHash, loopOut2)

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since two batches were created we check that it registered for its
	// primary sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Deliver the second sweep. It will go to a separate batch,
	// since CltvExpiry values are distant enough.
	require.NoError(t, batcher.AddSweep(&sweepReq2))
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Once batcher receives sweep request it will eventually spin up
	// batches.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored and we have exactly one
		// active batch.
		return batcherStore.AssertSweepStored(sweepReq1.SwapHash) &&
			batcherStore.AssertSweepStored(sweepReq2.SwapHash) &&
			len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Change the second sweep so that it can be added to the first batch.
	// Change CltvExpiry.
	loopOut2 = &loopdb.LoopOut{
		Loop: loopdb.Loop{
			Hash: lntypes.Hash{2, 2, 2},
		},
		Contract: &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      shortCltv,
				AmountRequested: 222,
				ProtocolVersion: loopdb.ProtocolVersionMuSig2,
				HtlcKeys:        htlcKeys,
			},
			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 111,
		},
	}
	store.putLoopOutSwap(sweepReq2.SwapHash, loopOut2)

	// Re-add the second sweep. It is expected to stay in second batch,
	// not added to both batches.
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	require.Eventually(t, func() bool {
		// Make sure there are two batches.
		batches := batcher.batches
		if len(batches) != 2 {
			return false
		}

		// Find the batch with largest ID. It must be the second batch.
		// Variable batches is a map, not a slice, so we have to visit
		// all the items and find the one with maximum id.
		var secondBatch *batch
		for _, batch := range batches {
			if secondBatch == nil || batch.id > secondBatch.id {
				secondBatch = batch
			}
		}

		// Make sure the second batch has the second sweep.
		sweep2, has := secondBatch.sweeps[sweepReq2.SwapHash]
		if !has {
			return false
		}

		// Make sure the second sweep's timeout has been updated.
		return sweep2.timeout == shortCltv
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure each batch has one sweep. If the second sweep was added to
	// both batches, the following check won't pass.
	for _, batch := range batcher.batches {
		require.Equal(t, 1, len(batch.sweeps))
	}

	// Publish a block to trigger batch 2 republishing.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for txs to be published.
	<-lnd.TxPublishChannel
	<-lnd.TxPublishChannel

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	checkBatcherError(t, runErr)
}

// testRestoringPreservesConfTarget tests that after the batch is written to DB
// and loaded back, its batchConfTarget value is preserved.
func testRestoringPreservesConfTarget(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Create a sweep request.
	sweepReq := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 123,
	}

	err = store.CreateLoopOut(ctx, sweepReq.SwapHash, swap)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored
		if !batcherStore.AssertSweepStored(sweepReq.SwapHash) {
			return false
		}

		// Make sure there is exactly one active batch.
		if len(batcher.batches) != 1 {
			return false
		}

		// Get the batch.
		batch := getOnlyBatch(batcher)

		// Make sure the batch has one sweep.
		if len(batch.sweeps) != 1 {
			return false
		}

		// Make sure the batch has proper batchConfTarget.
		return batch.cfg.batchConfTarget == 123
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure we have stored the batch.
	batches, err := batcherStore.FetchUnconfirmedSweepBatches(ctx)
	require.NoError(t, err)
	require.Len(t, batches, 1)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)

	// Now launch it again.
	batcher = NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	ctx, cancel = context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Wait for batch to load.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored
		if !batcherStore.AssertSweepStored(sweepReq.SwapHash) {
			return false
		}

		// Make sure there is exactly one active batch.
		if len(batcher.batches) != 1 {
			return false
		}

		// Get the batch.
		batch := getOnlyBatch(batcher)

		// Make sure the batch has one sweep.
		return len(batch.sweeps) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure batchConfTarget was preserved.
	require.Equal(t, 123, int(getOnlyBatch(batcher).cfg.batchConfTarget))

	// Expect registration for spend notification.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)
}

type sweepFetcherMock struct {
	store map[lntypes.Hash]*SweepInfo
}

func (f *sweepFetcherMock) FetchSweep(ctx context.Context, hash lntypes.Hash) (
	*SweepInfo, error) {

	return f.store[hash], nil
}

// testSweepFetcher tests providing custom sweep fetcher to Batcher.
func testSweepFetcher(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	// Extract payment address from the invoice.
	swapPaymentAddr, err := utils.ObtainSwapPaymentAddr(
		swapInvoice, lnd.ChainParams,
	)
	require.NoError(t, err)

	swapHash := lntypes.Hash{1, 1, 1}

	// Provide min fee rate for the sweep.
	feeRate := chainfee.SatPerKWeight(30000)
	amt := btcutil.Amount(1_000_000)
	weight := lntypes.WeightUnit(445) // Weight for 1-to-1 tx.
	expectedFee := feeRate.FeeForWeight(weight)

	swap := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      222,
			AmountRequested: amt,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},
		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 321,
	}

	htlc, err := utils.GetHtlc(
		swapHash, &swap.SwapContract, lnd.ChainParams,
	)
	require.NoError(t, err)

	sweepInfo := &SweepInfo{
		ConfTarget:             123,
		Timeout:                111,
		SwapInvoicePaymentAddr: *swapPaymentAddr,
		ProtocolVersion:        loopdb.ProtocolVersionMuSig2,
		HTLCKeys:               htlcKeys,
		HTLC:                   *htlc,
		HTLCSuccessEstimator:   htlc.AddSuccessToEstimator,
		DestAddr:               destAddr,
	}

	sweepFetcher := &sweepFetcherMock{
		store: map[lntypes.Hash]*SweepInfo{
			swapHash: sweepInfo,
		},
	}

	// Create a sweep request.
	sweepReq := SweepRequest{
		SwapHash: swapHash,
		Value:    amt,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	// Create a swap in the DB. It is needed to satisfy SQL constraints in
	// case of SQL test. The data is not actually used, since we pass sweep
	// fetcher, so put different conf target to make sure it is not used.
	err = store.CreateLoopOut(ctx, swapHash, swap)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	customFeeRate := func(ctx context.Context,
		swapHash lntypes.Hash) (chainfee.SatPerKWeight, error) {

		// Always provide the same value, no bumping.
		return feeRate, nil
	}

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepFetcher, WithCustomFeeRate(customFeeRate))

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored
		if !batcherStore.AssertSweepStored(swapHash) {
			return false
		}

		// Make sure there is exactly one active batch.
		if len(batcher.batches) != 1 {
			return false
		}

		// Get the batch.
		batch := getOnlyBatch(batcher)

		// Make sure the batch has one sweep.
		if len(batch.sweeps) != 1 {
			return false
		}

		// Make sure the batch has proper batchConfTarget.
		return batch.cfg.batchConfTarget == 123
	}, test.Timeout, eventuallyCheckFrequency)

	// Get the published transaction and check the fee rate.
	tx := <-lnd.TxPublishChannel
	out := btcutil.Amount(tx.TxOut[0].Value)
	gotFee := amt - out
	require.Equal(t, expectedFee, gotFee, "fees don't match")

	// Make sure we have stored the batch.
	batches, err := batcherStore.FetchUnconfirmedSweepBatches(ctx)
	require.NoError(t, err)
	require.Len(t, batches, 1)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)
}

// testSweepBatcherCloseDuringAdding tests that sweep batcher works correctly
// if it is closed (stops running) during AddSweep call.
func testSweepBatcherCloseDuringAdding(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Add many swaps.
	for i := byte(1); i < 255; i++ {
		swapHash := lntypes.Hash{i, i, i}

		// Create a swap contract.
		swap := &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      111,
				AmountRequested: 111,

				// Make preimage unique to pass SQL constraints.
				Preimage: lntypes.Preimage{i},
			},

			DestAddr:    destAddr,
			SwapInvoice: swapInvoice,
		}

		err = store.CreateLoopOut(ctx, swapHash, swap)
		require.NoError(t, err)
		store.AssertLoopOutStored()
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Add many sweeps.
		for i := byte(1); i < 255; i++ {
			// Create a sweep request.
			sweepReq := SweepRequest{
				SwapHash: lntypes.Hash{i, i, i},
				Value:    111,
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{i, i},
					Index: 1,
				},
				Notifier: &dummyNotifier,
			}

			// Deliver sweep request to batcher.
			err := batcher.AddSweep(&sweepReq)
			if err == ErrBatcherShuttingDown {
				break
			}
			require.NoError(t, err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Close sweepbatcher during addings.
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()

	// We don't know how many spend notification registrations will be
	// issued, so accept them while waiting for two goroutines to stop.
	quit := make(chan struct{})
	registrationChan := make(chan struct{})
	go func() {
		defer close(registrationChan)
		for {
			select {
			case <-lnd.RegisterSpendChannel:
			case <-quit:
				return
			}
		}
	}()

	wg.Wait()
	close(quit)
	<-registrationChan
}

// testCustomSignMuSig2 tests the operation with custom musig2 signer.
func testCustomSignMuSig2(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	// Use custom MuSig2 signer function.
	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		nil, testVerifySchnorrSig, lnd.ChainParams, batcherStore,
		sweepStore, WithCustomSignMuSig2(testSignMuSig2func))

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Create a sweep request.
	sweepReq := SweepRequest{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq.SwapHash, swap)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	tx := <-lnd.TxPublishChannel

	// Check the signature.
	gotSig := tx.TxIn[0].Witness[0]
	require.Equal(t, customSignature, gotSig, "signatures don't match")

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	checkBatcherError(t, runErr)
}

// TestSweepBatcherBatchCreation tests that sweep requests enter the expected
// batch based on their timeout distance.
func TestSweepBatcherBatchCreation(t *testing.T) {
	runTests(t, testSweepBatcherBatchCreation)
}

// TestFeeBumping tests that sweep is RBFed with slightly higher fee rate after
// each block unless WithCustomFeeRate is passed.
func TestFeeBumping(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		runTests(t, func(t *testing.T, store testStore,
			batcherStore testBatcherStore) {

			testFeeBumping(t, store, batcherStore, false)
		})
	})

	t.Run("fixed fee rate", func(t *testing.T) {
		runTests(t, func(t *testing.T, store testStore,
			batcherStore testBatcherStore) {

			testFeeBumping(t, store, batcherStore, true)
		})
	})
}

// TestSweepBatcherSimpleLifecycle tests the simple lifecycle of the batches
// that are created and run by the batcher.
func TestSweepBatcherSimpleLifecycle(t *testing.T) {
	runTests(t, testSweepBatcherSimpleLifecycle)
}

// TestSweepBatcherSweepReentry tests that when an old version of the batch tx
// gets confirmed the sweep leftovers are sent back to the batcher.
func TestSweepBatcherSweepReentry(t *testing.T) {
	runTests(t, testSweepBatcherSweepReentry)
}

// TestSweepBatcherNonWalletAddr tests that sweep requests that sweep to a non
// wallet address enter individual batches.
func TestSweepBatcherNonWalletAddr(t *testing.T) {
	runTests(t, testSweepBatcherNonWalletAddr)
}

// TestSweepBatcherComposite tests that sweep requests that sweep to both wallet
// addresses and non-wallet addresses enter the correct batches.
func TestSweepBatcherComposite(t *testing.T) {
	runTests(t, testSweepBatcherComposite)
}

// TestGetFeePortionForSweep tests that the fee portion for a sweep is correctly
// calculated.
func TestGetFeePortionForSweep(t *testing.T) {
	runTests(t, testGetFeePortionForSweep)
}

// TestRestoringEmptyBatch tests that the batcher can be restored with an empty
// batch.
func TestRestoringEmptyBatch(t *testing.T) {
	runTests(t, testRestoringEmptyBatch)
}

// TestHandleSweepTwice tests that handing the same sweep twice must not
// add it to different batches.
func TestHandleSweepTwice(t *testing.T) {
	runTests(t, testHandleSweepTwice)
}

// TestRestoringPreservesConfTarget tests that after the batch is written to DB
// and loaded back, its batchConfTarget value is preserved.
func TestRestoringPreservesConfTarget(t *testing.T) {
	runTests(t, testRestoringPreservesConfTarget)
}

// TestSweepFetcher tests providing custom sweep fetcher to Batcher.
func TestSweepFetcher(t *testing.T) {
	runTests(t, testSweepFetcher)
}

// TestSweepBatcherCloseDuringAdding tests that sweep batcher works correctly
// if it is closed (stops running) during AddSweep call.
func TestSweepBatcherCloseDuringAdding(t *testing.T) {
	runTests(t, testSweepBatcherCloseDuringAdding)
}

// TestCustomSignMuSig2 tests the operation with custom musig2 signer.
func TestCustomSignMuSig2(t *testing.T) {
	runTests(t, testCustomSignMuSig2)
}

// testBatcherStore is BatcherStore used in tests.
type testBatcherStore interface {
	BatcherStore

	// AssertSweepStored asserts that a sweep is stored.
	AssertSweepStored(id lntypes.Hash) bool
}

type loopdbBatcherStore struct {
	BatcherStore

	sweepsSet map[lntypes.Hash]struct{}
}

// UpsertSweep inserts a sweep into the database, or updates an existing sweep
// if it already exists. This wrapper was added to update sweepsSet.
func (s *loopdbBatcherStore) UpsertSweep(ctx context.Context,
	sweep *dbSweep) error {

	err := s.BatcherStore.UpsertSweep(ctx, sweep)
	if err == nil {
		s.sweepsSet[sweep.SwapHash] = struct{}{}
	}
	return err
}

// AssertSweepStored asserts that a sweep is stored.
func (s *loopdbBatcherStore) AssertSweepStored(id lntypes.Hash) bool {
	_, has := s.sweepsSet[id]
	return has
}

// testStore is loopdb used in tests.
type testStore interface {
	loopdb.SwapStore

	// AssertLoopOutStored asserts that a swap is stored.
	AssertLoopOutStored()
}

// loopdbStore wraps loopdb.SwapStore and implements testStore interface.
type loopdbStore struct {
	loopdb.SwapStore

	t *testing.T

	loopOutStoreChan chan struct{}
}

// newLoopdbStore creates new loopdbStore instance.
func newLoopdbStore(t *testing.T, swapStore loopdb.SwapStore) *loopdbStore {
	return &loopdbStore{
		SwapStore:        swapStore,
		t:                t,
		loopOutStoreChan: make(chan struct{}, 1),
	}
}

// CreateLoopOut adds an initiated swap to the store.
func (s *loopdbStore) CreateLoopOut(ctx context.Context, hash lntypes.Hash,
	swap *loopdb.LoopOutContract) error {

	err := s.SwapStore.CreateLoopOut(ctx, hash, swap)
	if err == nil {
		s.loopOutStoreChan <- struct{}{}
	}

	return err
}

// AssertLoopOutStored asserts that a swap is stored.
func (s *loopdbStore) AssertLoopOutStored() {
	s.t.Helper()

	select {
	case <-s.loopOutStoreChan:
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be stored")
	}
}

// runTests runs a test with both mock and loopdb.
func runTests(t *testing.T, testFn func(t *testing.T, store testStore,
	batcherStore testBatcherStore)) {

	logger := btclog.NewBackend(os.Stdout).Logger("SWEEP")
	logger.SetLevel(btclog.LevelTrace)
	UseLogger(logger)

	t.Run("mocks", func(t *testing.T) {
		store := loopdb.NewStoreMock(t)
		batcherStore := NewStoreMock()
		testFn(t, store, batcherStore)
	})

	t.Run("loopdb", func(t *testing.T) {
		sqlDB := loopdb.NewTestDB(t)
		typedSqlDB := loopdb.NewTypedStore[Querier](sqlDB)
		lnd := test.NewMockLnd()
		batcherStore := NewSQLStore(typedSqlDB, lnd.ChainParams)
		testStore := newLoopdbStore(t, sqlDB)
		testBatcherStore := &loopdbBatcherStore{
			BatcherStore: batcherStore,
			sweepsSet:    make(map[lntypes.Hash]struct{}),
		}
		testFn(t, testStore, testBatcherStore)
	})
}
