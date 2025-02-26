package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/assert"
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

// getBatches returns batches in thread-safe way.
func getBatches(ctx context.Context, batcher *Batcher) []*batch {
	var batches []*batch
	batcher.testRunInEventLoop(ctx, func() {
		for _, batch := range batcher.batches {
			batches = append(batches, batch)
		}
	})

	return batches
}

// tryGetOnlyBatch returns a single batch if there is exactly one batch, or nil.
func tryGetOnlyBatch(ctx context.Context, batcher *Batcher) *batch {
	batches := getBatches(ctx, batcher)

	if len(batches) == 1 {
		return batches[0]
	} else {
		return nil
	}
}

// getOnlyBatch makes sure the batcher has exactly one batch and returns it.
func getOnlyBatch(t *testing.T, ctx context.Context, batcher *Batcher) *batch {
	batches := getBatches(ctx, batcher)
	require.Len(t, batches, 1)

	return batches[0]
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
		return len(getBatches(ctx, batcher)) == 1
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
		return len(getBatches(ctx, batcher)) == 1
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

	// Since the second batch got created we check that it registered its
	// primary sweep's spend.
	<-lnd.RegisterSpendChannel

	// Batcher should create a second batch as timeout distance is greater
	// than the threshold
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps
		// in it.
		batches := getBatches(ctx, batcher)

		for _, batch := range batches {
			var bad bool

			batch.testRunInEventLoop(ctx, func() {
				switch batch.primarySweepID {
				case sweepReq1.SwapHash:
					if len(batch.sweeps) != 2 {
						bad = true
					}

				case sweepReq3.SwapHash:
					if len(batch.sweeps) != 1 {
						bad = true
					}
				}
			})

			if bad {
				return false
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

// walletKitWrapper wraps a wallet kit and memorizes the label of the most
// recent published transaction.
type walletKitWrapper struct {
	lndclient.WalletKitClient

	lastLabel string
}

// PublishTransaction publishes the transaction and memorizes its label.
func (w *walletKitWrapper) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, label string) error {

	w.lastLabel = label

	return w.WalletKitClient.PublishTransaction(ctx, tx, label)
}

// testTxLabeler tests transaction labels.
func testTxLabeler(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	walletKit := &walletKitWrapper{WalletKitClient: lnd.WalletKit}

	batcher := NewBatcher(walletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore)

	var (
		runErr error
		wg     sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
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

	// When batch is successfully created it will execute it's first step,
	// which leads to a spend monitor of the primary sweep.
	<-lnd.RegisterSpendChannel

	// Eventually request will be consumed and a new batch will spin up.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Find the batch and assign it to a local variable for easier access.
	var theBatch *batch
	for _, btch := range getBatches(ctx, batcher) {
		btch.testRunInEventLoop(ctx, func() {
			if btch.primarySweepID == sweepReq1.SwapHash {
				theBatch = btch
			}
		})
	}

	// Now test the label.
	wantLabel := fmt.Sprintf("BatchOutSweepSuccess -- %d", theBatch.id)
	require.Equal(t, wantLabel, walletKit.lastLabel)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()
	checkBatcherError(t, runErr)

	// Define dummy tx labeler, always returning "test".
	txLabeler := func(batchID int32) string {
		return "test"
	}

	// Now try it with option WithTxLabeler.
	batcher = NewBatcher(walletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithTxLabeler(txLabeler))

	ctx, cancel = context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Expect batch to register for spending.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Now test the label.
	require.Equal(t, "test", walletKit.lastLabel)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()
	checkBatcherError(t, runErr)
}

// testTransactionPublisher wraps a wallet kit and returns publish error on
// the first publish attempt. Further attempts succeed.
type testTransactionPublisher struct {
	lndclient.WalletKitClient

	attempts int
}

var testPublishError = errors.New("test publish error")

// PublishTransaction publishes the transaction or fails it's the first attempt.
func (p *testTransactionPublisher) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, label string) error {

	p.attempts++
	if p.attempts == 1 {
		return testPublishError
	}

	return p.WalletKitClient.PublishTransaction(ctx, tx, label)
}

// testPublishErrorHandler tests that publish error handler installed with
// WithPublishErrorHandler, works as expected.
func testPublishErrorHandler(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	walletKit := &testTransactionPublisher{WalletKitClient: lnd.WalletKit}

	// Catch all publish errors and send them to a channel.
	publishErrorChan := make(chan error)
	errorHandler := func(err error, errMsg string, log btclog.Logger) {
		log.Warnf("%s: %v", errMsg, err)

		publishErrorChan <- err
	}

	batcher := NewBatcher(walletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithPublishErrorHandler(errorHandler))

	var (
		runErr error
		wg     sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
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

	// When batch is successfully created it will execute it's first step,
	// which leads to a spend monitor of the primary sweep.
	<-lnd.RegisterSpendChannel

	// Eventually request will be consumed and a new batch will spin up.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// The first attempt to publish the batch tx is expected to fail.
	require.ErrorIs(t, <-publishErrorChan, testPublishError)

	// Mine a block to trigger another publishing attempt.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Now publishing should succeed for tx to be published.
	<-lnd.TxPublishChannel

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()
	checkBatcherError(t, runErr)
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

	// When batch is successfully created it will execute it's first step,
	// which leads to a spend monitor of the primary sweep.
	<-lnd.RegisterSpendChannel

	// Eventually request will be consumed and a new batch will spin up.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Find the batch and assign it to a local variable for easier access.
	batch := &batch{}
	for _, btch := range getBatches(ctx, batcher) {
		btch.testRunInEventLoop(ctx, func() {
			if btch.primarySweepID == sweepReq1.SwapHash {
				batch = btch
			}
		})
	}

	require.Eventually(t, func() bool {
		// Batch should have the sweep stored.
		var numSweeps int
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
		})

		return numSweeps == 1
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
		var currentHeight int32
		batch.testRunInEventLoop(ctx, func() {
			currentHeight = batch.currentHeight
		})

		return currentHeight == 601
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
		var state batchState
		batch.testRunInEventLoop(ctx, func() {
			state = batch.state
		})

		return state == Closed
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
		var complete bool
		batch.testRunInEventLoop(ctx, func() {
			complete = batch.isComplete()
		})

		return complete
	}, test.Timeout, eventuallyCheckFrequency)
}

// wrappedLogger implements btclog.Logger, recording last debug message format.
// It is needed to watch for messages in tests.
type wrappedLogger struct {
	btclog.Logger

	mu sync.Mutex

	debugMessages []string
	infoMessages  []string
}

// Debugf logs debug message.
func (l *wrappedLogger) Debugf(format string, params ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.debugMessages = append(l.debugMessages, format)
	l.Logger.Debugf(format, params...)
}

// Infof logs info message.
func (l *wrappedLogger) Infof(format string, params ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.infoMessages = append(l.infoMessages, format)
	l.Logger.Infof(format, params...)
}

// testDelays tests that WithInitialDelay and WithPublishDelay work.
func testDelays(t *testing.T, store testStore, batcherStore testBatcherStore) {
	// Set initial delay and publish delay.
	const (
		initialDelay = 4 * time.Second
		publishDelay = 3 * time.Second
	)

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	startTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)
	tickSignal := make(chan time.Duration)
	testClock := clock.NewTestClockWithTickSignal(startTime, tickSignal)

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithInitialDelay(initialDelay),
		WithPublishDelay(publishDelay), WithClock(testClock),
	)

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
			CltvExpiry:      1000,
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

	// Expect two timers to be set: initialDelay and publishDelay,
	// and RegisterSpend to be called. The order is not determined,
	// so catch these actions from two separate goroutines.
	var wg2 sync.WaitGroup

	wg2.Add(1)
	go func() {
		defer wg2.Done()

		// Since a batch was created we check that it registered for its
		// primary sweep's spend.
		<-lnd.RegisterSpendChannel
	}()

	wg2.Add(1)
	var delays []time.Duration
	go func() {
		defer wg2.Done()

		// Expect two timers: initialDelay and publishDelay.
		delays = append(delays, <-tickSignal)
		delays = append(delays, <-tickSignal)
	}()

	// Wait for RegisterSpend and for timer registrations.
	wg2.Wait()

	// Expect timer for initialDelay and publishDelay to be registered.
	wantDelays := []time.Duration{initialDelay, publishDelay}
	require.Equal(t, wantDelays, delays)

	// Eventually the batch is launched.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Replace the logger in the batch with wrappedLogger to watch messages.
	batch1 := getOnlyBatch(t, ctx, batcher)
	var testLogger *wrappedLogger
	batch1.testRunInEventLoop(ctx, func() {
		testLogger = &wrappedLogger{
			Logger: batch1.log(),
		}
		batch1.setLog(testLogger)
	})

	// Advance the clock to publishDelay. It will trigger the publishDelay
	// timer, but won't result in publishing, because of initialDelay.
	now := startTime.Add(publishDelay)
	testClock.SetTime(now)

	// Wait for batch publishing to be skipped, because initialDelay has not
	// ended.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		testLogger.mu.Lock()
		defer testLogger.mu.Unlock()

		assert.Contains(c, testLogger.debugMessages, stillWaitingMsg)
	}, test.Timeout, eventuallyCheckFrequency)

	// Advance the clock to the end of initialDelay.
	now = startTime.Add(initialDelay)
	testClock.SetTime(now)

	// Expect timer for publishDelay to be registered.
	require.Equal(t, publishDelay, <-tickSignal)

	// Advance the clock.
	now = now.Add(publishDelay)
	testClock.SetTime(now)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored
		if !batcherStore.AssertSweepStored(sweepReq.SwapHash) {
			return false
		}

		batch := tryGetOnlyBatch(ctx, batcher)
		if batch == nil {
			return false
		}

		// Make sure the batch has one sweep.
		var numSweeps int
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
		})

		// Make sure the batch has one sweep.
		return numSweeps == 1
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

	// Advance the clock by 1 second.
	now = now.Add(time.Second)
	testClock.SetTime(now)

	// Now launch it again.
	batcher = NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithInitialDelay(initialDelay),
		WithPublishDelay(publishDelay), WithClock(testClock),
	)
	ctx, cancel = context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Expect a timer to be set: 0 (instead of publishDelay), and
	// RegisterSpend to be called. The order is not determined, so catch
	// these actions from two separate goroutines.
	var wg3 sync.WaitGroup

	wg3.Add(1)
	go func() {
		defer wg3.Done()

		// Since a batch was created we check that it registered for its
		// primary sweep's spend.
		<-lnd.RegisterSpendChannel

		// Wait for tx to be published.
		<-lnd.TxPublishChannel
	}()

	wg3.Add(1)
	delays = nil
	go func() {
		defer wg3.Done()

		// Expect one timer: publishDelay (0).
		delays = append(delays, <-tickSignal)
	}()

	// Wait for RegisterSpend and for timer registration.
	wg3.Wait()

	// Wait for batch to load.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored
		if !batcherStore.AssertSweepStored(sweepReq.SwapHash) {
			return false
		}

		batch := tryGetOnlyBatch(ctx, batcher)
		if batch == nil {
			return false
		}

		// Make sure the batch has one sweep.
		var numSweeps int
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
		})

		// Make sure the batch has one sweep.
		return numSweeps == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Expect one timer: publishDelay (0).
	wantDelays = []time.Duration{0}
	require.Equal(t, wantDelays, delays)

	// Advance the clock.
	now = now.Add(time.Millisecond)
	testClock.SetTime(now)

	// Tick tock next block.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Expect timer for publishDelay (0) to be registered. Make sure
	// sweepbatcher does not wait for recovered batches after new block
	// arrives as well.
	require.Equal(t, time.Duration(0), <-tickSignal)

	// Advance the clock.
	now = now.Add(time.Millisecond)
	testClock.SetTime(now)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)

	// Advance the clock by 1 second.
	now = now.Add(time.Second)
	testClock.SetTime(now)

	// Now test for large initialDelay and make sure it is cancelled
	// for an urgent sweep.
	const largeInitialDelay = 6 * time.Hour

	batcher = NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithInitialDelay(largeInitialDelay),
		WithPublishDelay(publishDelay), WithClock(testClock),
	)
	ctx, cancel = context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Expect spend notification and publication for the first batch.
	// Expect a timer to be set: 0 (instead of publishDelay), and
	// RegisterSpend to be called. The order is not determined, so catch
	// these actions from two separate goroutines.
	var wg4 sync.WaitGroup

	wg4.Add(1)
	go func() {
		defer wg4.Done()

		// Since a batch was created we check that it registered for its
		// primary sweep's spend.
		<-lnd.RegisterSpendChannel
	}()

	wg4.Add(1)
	delays = nil
	go func() {
		defer wg4.Done()

		// Expect one timer: publishDelay (0).
		delays = append(delays, <-tickSignal)
	}()

	// Wait for RegisterSpend and for timer registration.
	wg4.Wait()

	// Expect one timer: publishDelay (0).
	wantDelays = []time.Duration{0}
	require.Equal(t, wantDelays, delays)

	// Get spend notification and tx publication for the first batch.
	<-lnd.TxPublishChannel

	// Create a sweep request which is not urgent, but close to.
	sweepReq2 := SweepRequest{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	const blocksInDelay = int32(largeInitialDelay / (10 * time.Minute))
	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			// CltvExpiry is not urgent, but close.
			CltvExpiry: 600 + blocksInDelay*2 + 5,

			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 123,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Expect the sweep to be added to new batch. Expect two timers:
	// largeInitialDelay and publishDelay. RegisterSpend is called in
	// parallel, so catch these actions from two separate goroutines.
	var wg5 sync.WaitGroup

	wg5.Add(1)
	go func() {
		defer wg5.Done()

		// Since a batch was created we check that it registered for its
		// primary sweep's spend.
		<-lnd.RegisterSpendChannel
	}()

	wg5.Add(1)
	delays = nil
	go func() {
		defer wg5.Done()

		// Expect two timer: largeInitialDelay, publishDelay.
		delays = append(delays, <-tickSignal)
		delays = append(delays, <-tickSignal)
	}()

	// Wait for RegisterSpend and for timers' registrations.
	wg5.Wait()

	// Expect two timers: largeInitialDelay, publishDelay.
	wantDelays = []time.Duration{largeInitialDelay, publishDelay}
	require.Equal(t, wantDelays, delays)

	// Replace the logger in the batch with wrappedLogger to watch messages.
	var testLogger2 *wrappedLogger
	for _, batch := range getBatches(ctx, batcher) {
		batch.testRunInEventLoop(ctx, func() {
			if batch.id != batch1.id {
				testLogger2 = &wrappedLogger{
					Logger: batch.log(),
				}
				batch.setLog(testLogger2)
			}
		})
	}
	require.NotNil(t, testLogger2)

	// Add another sweep which is urgent. It will go to the same batch
	// to make sure minimum timeout is calculated properly.
	sweepReq3 := SweepRequest{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}
	swap3 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			// CltvExpiry is urgent.
			CltvExpiry: 600 + blocksInDelay*2 - 5,

			AmountRequested: 111,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{3},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 123,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Wait for sweep to be added to the batch.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		testLogger2.mu.Lock()
		defer testLogger2.mu.Unlock()

		assert.Contains(c, testLogger2.infoMessages, "adding sweep %x")
	}, test.Timeout, eventuallyCheckFrequency)

	// Advance the clock by publishDelay. Don't wait largeInitialDelay.
	now = now.Add(publishDelay)
	testClock.SetTime(now)

	// Wait for tx to be published.
	tx := <-lnd.TxPublishChannel
	require.Equal(t, 2, len(tx.TxIn))

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)
}

// testMaxSweepsPerBatch tests the limit on max number of sweeps per batch.
func testMaxSweepsPerBatch(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	// Disable logging, because this test is very noisy.
	oldLogger := log()
	UseLogger(build.NewSubLogger("SWEEP", nil))
	defer UseLogger(oldLogger)

	defer test.Guard(t, test.WithGuardTimeout(5*time.Minute))()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	startTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)
	testClock := clock.NewTestClock(startTime)

	// Create muSig2SignSweep failing all sweeps to force non-cooperative
	// scenario (it increases transaction size).
	muSig2SignSweep := func(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
		prevoutMap map[wire.OutPoint]*wire.TxOut) (
		[]byte, []byte, error) {

		return nil, nil, fmt.Errorf("test error")
	}

	// Set publish delay.
	const publishDelay = 3 * time.Second

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		muSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithPublishDelay(publishDelay),
		WithClock(testClock),
	)

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	const swapsNum = MaxSweepsPerBatch + 1

	// Expect 2 batches to be registered.
	expectedBatches := (swapsNum + MaxSweepsPerBatch - 1) /
		MaxSweepsPerBatch

	for i := 0; i < swapsNum; i++ {
		preimage := lntypes.Preimage{2, byte(i % 256), byte(i / 256)}
		swapHash := preimage.Hash()

		// Create a sweep request.
		sweepReq := SweepRequest{
			SwapHash: swapHash,
			Value:    111,
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{1, 1},
				Index: 1,
			},
			Notifier: &dummyNotifier,
		}

		swap := &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      1000,
				AmountRequested: 111,
				ProtocolVersion: loopdb.ProtocolVersionMuSig2,
				HtlcKeys:        htlcKeys,

				// Make preimage unique to pass SQL constraints.
				Preimage: preimage,
			},

			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 123,
		}

		err = store.CreateLoopOut(ctx, swapHash, swap)
		require.NoError(t, err)
		store.AssertLoopOutStored()

		// Deliver sweep request to batcher.
		require.NoError(t, batcher.AddSweep(&sweepReq))

		// If this is new batch, expect a spend registration.
		if i%MaxSweepsPerBatch == 0 {
			<-lnd.RegisterSpendChannel
		}
	}

	// Eventually the batches are launched and all the sweeps are added.
	require.Eventually(t, func() bool {
		// Make sure all the batches have started.
		batches := getBatches(ctx, batcher)
		if len(batches) != expectedBatches {
			return false
		}

		// Make sure all the sweeps were added.
		sweepsNum := 0
		for _, batch := range batches {
			batch.testRunInEventLoop(ctx, func() {
				sweepsNum += len(batch.sweeps)
			})
		}

		return sweepsNum == swapsNum
	}, test.Timeout, eventuallyCheckFrequency)

	// Advance the clock to publishDelay, so batches are published.
	now := startTime.Add(publishDelay)
	testClock.SetTime(now)

	// Expect mockSigner.SignOutputRaw calls to sign non-cooperative
	// sweeps.
	for i := 0; i < expectedBatches; i++ {
		<-lnd.SignOutputRawChannel
	}

	// Wait for txs to be published.
	inputsNum := 0
	const maxWeight = lntypes.WeightUnit(400_000)
	for i := 0; i < expectedBatches; i++ {
		tx := <-lnd.TxPublishChannel
		inputsNum += len(tx.TxIn)

		// Make sure the transaction size is standard.
		weight := lntypes.WeightUnit(
			blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
		)
		require.Less(t, weight, maxWeight)
		t.Logf("tx weight: %v", weight)
	}

	// Make sure the number of inputs in batch transactions is equal
	// to the number of swaps.
	require.Equal(t, swapsNum, inputsNum)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)
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
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Find the batch and store it in a local variable for easier access.
	b := &batch{}
	for _, btch := range getBatches(ctx, batcher) {
		btch.testRunInEventLoop(ctx, func() {
			if btch.primarySweepID == sweepReq1.SwapHash {
				b = btch
			}
		})
	}

	// Batcher should contain all sweeps.
	require.Eventually(t, func() bool {
		var numSweeps int
		b.testRunInEventLoop(ctx, func() {
			numSweeps = len(b.sweeps)
		})

		return numSweeps == 3
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
		var state batchState
		b.testRunInEventLoop(ctx, func() {
			state = b.state
		})

		return state == Closed
	}, test.Timeout, eventuallyCheckFrequency)

	// Since second batch was created we check that it registered for its
	// primary sweep's spend.
	<-lnd.RegisterSpendChannel

	// While handling the spend notification the batch should detect that
	// some sweeps did not appear in the spending tx, therefore it redirects
	// them back to the batcher and the batcher inserts them in a new batch.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 2
	}, test.Timeout, eventuallyCheckFrequency)

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

	// Re-add one of remaining sweeps to trigger removing the completed
	// batch from the batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Eventually the batch receives the confirmation notification,
	// gracefully exits and the batcher deletes it.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Find the other batch, which includes the sweeps that did not appear
	// in the spending tx.
	b = getOnlyBatch(t, ctx, batcher)

	// After all the sweeps enter, it should contain 2 sweeps.
	require.Eventually(t, func() bool {
		var numSweeps int
		b.testRunInEventLoop(ctx, func() {
			numSweeps = len(b.sweeps)
		})
		return numSweeps == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// The batch should be in an open state.
	var state batchState
	b.testRunInEventLoop(ctx, func() {
		state = b.state
	})
	require.Equal(t, state, Open)
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

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

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

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Batcher should create a second batch as first batch is a non wallet
	// addr batch.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 2
	}, test.Timeout, eventuallyCheckFrequency)

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

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Batcher should create a new batch as timeout distance is greater than
	// the threshold
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx to be published for 3rd batch.
	<-lnd.TxPublishChannel

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps
		// in it.
		batches := getBatches(ctx, batcher)
		for _, batch := range batches {
			var bad bool

			batch.testRunInEventLoop(ctx, func() {
				switch batch.primarySweepID {
				case sweepReq1.SwapHash:
					if len(batch.sweeps) != 1 {
						bad = true
					}

				case sweepReq2.SwapHash:
					if len(batch.sweeps) != 1 {
						bad = true
					}

				case sweepReq3.SwapHash:
					if len(batch.sweeps) != 1 {
						bad = true
					}
				}
			})

			if bad {
				return false
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

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Insert the same swap twice, this should be a noop.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Batcher should not create a second batch as timeout distance is small
	// enough.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Publish a block to trigger batch 1 republishing.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for tx for the first batch to be published (2 sweeps).
	tx := <-lnd.TxPublishChannel
	require.Equal(t, 2, len(tx.TxIn))

	require.NoError(t, batcher.AddSweep(&sweepReq3))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Batcher should create a second batch as this sweep pays to a non
	// wallet address.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx for the second batch to be published (1 sweep).
	tx = <-lnd.TxPublishChannel
	require.Equal(t, 1, len(tx.TxIn))

	require.NoError(t, batcher.AddSweep(&sweepReq4))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Batcher should create a third batch as timeout distance is greater
	// than the threshold.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 3
	}, test.Timeout, eventuallyCheckFrequency)

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
		return len(getBatches(ctx, batcher)) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	require.NoError(t, batcher.AddSweep(&sweepReq6))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Batcher should create a fourth batch as this sweep pays to a non
	// wallet address.
	require.Eventually(t, func() bool {
		return len(getBatches(ctx, batcher)) == 4
	}, test.Timeout, eventuallyCheckFrequency)

	// Wait for tx for the 4th batch to be published (1 sweep).
	tx = <-lnd.TxPublishChannel
	require.Equal(t, 1, len(tx.TxIn))

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps in
		// it.
		batches := getBatches(ctx, batcher)
		for _, batch := range batches {
			var bad bool
			batch.testRunInEventLoop(ctx, func() {
				switch batch.primarySweepID {
				case sweepReq1.SwapHash:
					if len(batch.sweeps) != 2 {
						bad = true
					}

				case sweepReq3.SwapHash:
					if len(batch.sweeps) != 1 {
						bad = true
					}

				case sweepReq4.SwapHash:
					if len(batch.sweeps) != 2 {
						bad = true
					}

				case sweepReq6.SwapHash:
					if len(batch.sweeps) != 1 {
						bad = true
					}
				}
			})

			if bad {
				return false
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
		if !batcherStore.AssertSweepStored(sweepReq.SwapHash) {
			return false
		}

		return len(getBatches(ctx, batcher)) == 1
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
		if !batcherStore.AssertSweepStored(sweepReq1.SwapHash) {
			return false
		}
		if !batcherStore.AssertSweepStored(sweepReq2.SwapHash) {
			return false
		}

		return len(getBatches(ctx, batcher)) == 2
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
		batches := getBatches(ctx, batcher)

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
		var (
			sweep2 sweep
			has    bool
		)
		secondBatch.testRunInEventLoop(ctx, func() {
			sweep2, has = secondBatch.sweeps[sweepReq2.SwapHash]
		})
		if !has {
			return false
		}

		// Make sure the second sweep's timeout has been updated.
		return sweep2.timeout == shortCltv
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure each batch has one sweep. If the second sweep was added to
	// both batches, the following check won't pass.
	batches := getBatches(ctx, batcher)
	for _, batch := range batches {
		var numSweeps int
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
		})

		// Make sure the batch has one sweep.
		require.Equal(t, 1, numSweeps)
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

		batch := tryGetOnlyBatch(ctx, batcher)
		if batch == nil {
			return false
		}

		// Make sure the batch has one sweep.
		var (
			numSweeps  int
			confTarget int32
		)
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
			confTarget = batch.cfg.batchConfTarget
		})

		// Make sure the batch has one sweep.
		if numSweeps != 1 {
			return false
		}

		// Make sure the batch has proper batchConfTarget.
		return confTarget == 123
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

	// Expect registration for spend notification.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Wait for batch to load.
	require.Eventually(t, func() bool {
		// Make sure that the sweep was stored
		if !batcherStore.AssertSweepStored(sweepReq.SwapHash) {
			return false
		}

		batch := tryGetOnlyBatch(ctx, batcher)
		if batch == nil {
			return false
		}

		// Make sure the batch has one sweep.
		var numSweeps int
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
		})

		// Make sure the batch has one sweep.
		return numSweeps == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure batchConfTarget was preserved.
	batch := getOnlyBatch(t, ctx, batcher)
	var confTarget int32
	batch.testRunInEventLoop(ctx, func() {
		confTarget = batch.cfg.batchConfTarget
	})
	require.Equal(t, int32(123), confTarget)

	// Now make the batcher quit by canceling the context.
	cancel()
	wg.Wait()

	// Make sure the batcher exited without an error.
	checkBatcherError(t, runErr)
}

type sweepFetcherMock struct {
	store map[lntypes.Hash]*SweepInfo
	mu    sync.Mutex
}

func (f *sweepFetcherMock) setSweep(hash lntypes.Hash, info *SweepInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.store[hash] = info
}

func (f *sweepFetcherMock) FetchSweep(ctx context.Context, hash lntypes.Hash) (
	*SweepInfo, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

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
	weight := lntypes.WeightUnit(396) // Weight for 1-to-1 tx.
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
		nil, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepFetcher, WithCustomFeeRate(customFeeRate),
		WithCustomSignMuSig2(testSignMuSig2func))

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

		// Try to get the batch.
		batch := tryGetOnlyBatch(ctx, batcher)
		if batch == nil {
			return false
		}

		// Make sure the batch has one sweep.
		var (
			numSweeps  int
			confTarget int32
		)
		batch.testRunInEventLoop(ctx, func() {
			numSweeps = len(batch.sweeps)
			confTarget = batch.cfg.batchConfTarget
		})
		if numSweeps != 1 {
			return false
		}

		// Make sure the batch has proper batchConfTarget.
		return confTarget == 123
	}, test.Timeout, eventuallyCheckFrequency)

	// Get the published transaction and check the fee rate.
	tx := <-lnd.TxPublishChannel
	out := btcutil.Amount(tx.TxOut[0].Value)
	gotFee := amt - out
	require.Equal(t, expectedFee, gotFee, "fees don't match")
	gotWeight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
	)
	require.Equal(t, weight, gotWeight, "weights don't match")
	gotFeeRate := chainfee.NewSatPerKWeight(gotFee, gotWeight)
	require.Equal(t, feeRate, gotFeeRate, "fee rates don't match")

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

			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 111,
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

// testWithMixedBatch tests mixed batches construction. It also tests
// non-cooperative sweeping (using a preimage). Sweeps are added one by one.
func testWithMixedBatch(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	// Extract payment address from the invoice.
	swapPaymentAddr, err := utils.ObtainSwapPaymentAddr(
		swapInvoice, lnd.ChainParams,
	)
	require.NoError(t, err)

	// Use sweepFetcher to provide NonCoopHint for swapHash1.
	sweepFetcher := &sweepFetcherMock{
		store: map[lntypes.Hash]*SweepInfo{},
	}

	// Create 3 sweeps:
	//  1. known in advance to be non-cooperative,
	//  2. fails cosigning during an attempt,
	//  3. co-signs successfully.

	// Create 3 preimages, for 3 sweeps.
	var preimages = []lntypes.Preimage{
		{1},
		{2},
		{3},
	}

	// Swap hashes must match the preimages, for non-cooperative spending
	// path to work.
	var swapHashes = []lntypes.Hash{
		preimages[0].Hash(),
		preimages[1].Hash(),
		preimages[2].Hash(),
	}

	// Create muSig2SignSweep working only for 3rd swapHash.
	muSig2SignSweep := func(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
		prevoutMap map[wire.OutPoint]*wire.TxOut) (
		[]byte, []byte, error) {

		if swapHash == swapHashes[2] {
			return nil, nil, nil
		} else {
			return nil, nil, fmt.Errorf("test error")
		}
	}

	// Use mixed batches.
	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		muSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepFetcher,
	)

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Expected weights for transaction having 1, 2, and 3 sweeps.
	wantWeights := []lntypes.WeightUnit{559, 952, 1182}

	// Two non-cooperative sweeps, one cooperative.
	wantWitnessSizes := []int{4, 4, 1}

	// Create 3 swaps and 3 sweeps.
	for i, swapHash := range swapHashes {
		// Publish a block to trigger republishing.
		err = lnd.NotifyHeight(601 + int32(i))
		require.NoError(t, err)

		// Put a swap into store to satisfy SQL constraints.
		swap := &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      111,
				AmountRequested: 1_000_000,
				ProtocolVersion: loopdb.ProtocolVersionMuSig2,
				HtlcKeys:        htlcKeys,

				// Make preimage unique to pass SQL constraints.
				Preimage: preimages[i],
			},

			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 111,
		}

		require.NoError(t, store.CreateLoopOut(ctx, swapHash, swap))
		store.AssertLoopOutStored()

		// Add SweepInfo to sweepFetcher.
		htlc, err := utils.GetHtlc(
			swapHash, &swap.SwapContract, lnd.ChainParams,
		)
		require.NoError(t, err)

		sweepInfo := &SweepInfo{
			Preimage:               preimages[i],
			ConfTarget:             123,
			Timeout:                111,
			SwapInvoicePaymentAddr: *swapPaymentAddr,
			ProtocolVersion:        loopdb.ProtocolVersionMuSig2,
			HTLCKeys:               htlcKeys,
			HTLC:                   *htlc,
			HTLCSuccessEstimator:   htlc.AddSuccessToEstimator,
			DestAddr:               destAddr,
		}
		// The first sweep is known in advance to be non-cooperative.
		if i == 0 {
			sweepInfo.NonCoopHint = true
		}
		sweepFetcher.setSweep(swapHash, sweepInfo)

		// Create sweep request.
		sweepReq := SweepRequest{
			SwapHash: swapHash,
			Value:    1_000_000,
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{1, 1},
				Index: 1,
			},
			Notifier: &dummyNotifier,
		}
		require.NoError(t, batcher.AddSweep(&sweepReq))

		if i == 0 {
			// Since a batch was created we check that it registered
			// for its primary sweep's spend.
			<-lnd.RegisterSpendChannel
		}

		// Expect mockSigner.SignOutputRaw call to sign non-cooperative
		// sweeps.
		<-lnd.SignOutputRawChannel

		// A transaction is published.
		tx := <-lnd.TxPublishChannel
		require.Equal(t, i+1, len(tx.TxIn))

		// Check types of inputs.
		var witnessSizes []int
		for _, txIn := range tx.TxIn {
			witnessSizes = append(witnessSizes, len(txIn.Witness))
		}
		// The order of inputs is not deterministic, because they
		// are stored in map.
		require.ElementsMatch(t, wantWitnessSizes[:i+1], witnessSizes)

		// Calculate expected values.
		feeRate := test.DefaultMockFee
		for range i {
			// Bump fee the number of blocks passed.
			feeRate += defaultFeeRateStep
		}
		amt := btcutil.Amount(1_000_000 * (i + 1))
		weight := wantWeights[i]
		expectedFee := feeRate.FeeForWeight(weight)

		// Check weight.
		gotWeight := lntypes.WeightUnit(
			blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
		)
		require.Equal(t, weight, gotWeight, "weights don't match")

		// Check fee.
		out := btcutil.Amount(tx.TxOut[0].Value)
		gotFee := amt - out
		require.Equal(t, expectedFee, gotFee, "fees don't match")

		// Check fee rate.
		gotFeeRate := chainfee.NewSatPerKWeight(gotFee, gotWeight)
		require.Equal(t, feeRate, gotFeeRate, "fee rates don't match")
	}

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

// testWithMixedBatchCustom tests mixed batches construction, custom scenario.
// All sweeps are added at once.
func testWithMixedBatchCustom(t *testing.T, store testStore,
	batcherStore testBatcherStore, preimages []lntypes.Preimage,
	muSig2SignSweep MuSig2SignSweep, nonCoopHints []bool,
	expectSignOutputRawChannel bool, wantWeight lntypes.WeightUnit,
	wantWitnessSizes []int) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	// Extract payment address from the invoice.
	swapPaymentAddr, err := utils.ObtainSwapPaymentAddr(
		swapInvoice, lnd.ChainParams,
	)
	require.NoError(t, err)

	// Use sweepFetcher to provide NonCoopHint for swapHash1.
	sweepFetcher := &sweepFetcherMock{
		store: map[lntypes.Hash]*SweepInfo{},
	}

	// Swap hashes must match the preimages, for non-cooperative spending
	// path to work.
	swapHashes := make([]lntypes.Hash, len(preimages))
	for i, preimage := range preimages {
		swapHashes[i] = preimage.Hash()
	}

	// Use mixed batches.
	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		muSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepFetcher,
	)

	var wg sync.WaitGroup
	wg.Add(1)

	var runErr error
	go func() {
		defer wg.Done()
		runErr = batcher.Run(ctx)
	}()

	// Wait for the batcher to be initialized.
	<-batcher.initDone

	// Create swaps and sweeps.
	for i, swapHash := range swapHashes {
		// Put a swap into store to satisfy SQL constraints.
		swap := &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				CltvExpiry:      111,
				AmountRequested: 1_000_000,
				ProtocolVersion: loopdb.ProtocolVersionMuSig2,
				HtlcKeys:        htlcKeys,

				// Make preimage unique to pass SQL constraints.
				Preimage: preimages[i],
			},

			DestAddr:        destAddr,
			SwapInvoice:     swapInvoice,
			SweepConfTarget: 111,
		}

		require.NoError(t, store.CreateLoopOut(ctx, swapHash, swap))
		store.AssertLoopOutStored()

		// Add SweepInfo to sweepFetcher.
		htlc, err := utils.GetHtlc(
			swapHash, &swap.SwapContract, lnd.ChainParams,
		)
		require.NoError(t, err)

		sweepFetcher.setSweep(swapHash, &SweepInfo{
			Preimage:    preimages[i],
			NonCoopHint: nonCoopHints[i],

			ConfTarget:             123,
			Timeout:                111,
			SwapInvoicePaymentAddr: *swapPaymentAddr,
			ProtocolVersion:        loopdb.ProtocolVersionMuSig2,
			HTLCKeys:               htlcKeys,
			HTLC:                   *htlc,
			HTLCSuccessEstimator:   htlc.AddSuccessToEstimator,
			DestAddr:               destAddr,
		})

		// Create sweep request.
		sweepReq := SweepRequest{
			SwapHash: swapHash,
			Value:    1_000_000,
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{1, 1},
				Index: 1,
			},
			Notifier: &dummyNotifier,
		}
		require.NoError(t, batcher.AddSweep(&sweepReq))

		if i == 0 {
			// Since a batch was created we check that it registered
			// for its primary sweep's spend.
			<-lnd.RegisterSpendChannel
		}
	}

	if expectSignOutputRawChannel {
		// Expect mockSigner.SignOutputRaw call to sign non-cooperative
		// sweeps.
		<-lnd.SignOutputRawChannel
	}

	// A transaction is published.
	tx := <-lnd.TxPublishChannel
	require.Equal(t, len(preimages), len(tx.TxIn))

	// Check types of inputs.
	var witnessSizes []int
	for _, txIn := range tx.TxIn {
		witnessSizes = append(witnessSizes, len(txIn.Witness))
	}
	// The order of inputs is not deterministic, because they
	// are stored in map.
	require.ElementsMatch(t, wantWitnessSizes, witnessSizes)

	// Calculate expected values.
	feeRate := test.DefaultMockFee
	amt := btcutil.Amount(1_000_000 * len(preimages))
	expectedFee := feeRate.FeeForWeight(wantWeight)

	// Check weight.
	gotWeight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
	)
	require.Equal(t, wantWeight, gotWeight, "weights don't match")

	// Check fee.
	out := btcutil.Amount(tx.TxOut[0].Value)
	gotFee := amt - out
	require.Equal(t, expectedFee, gotFee, "fees don't match")

	// Check fee rate.
	gotFeeRate := chainfee.NewSatPerKWeight(gotFee, gotWeight)
	require.Equal(t, feeRate, gotFeeRate, "fee rates don't match")

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

// testWithMixedBatchLarge tests mixed batches construction, many sweeps.
// All sweeps are added at once.
func testWithMixedBatchLarge(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	// Create 9 sweeps. 3 groups of 3 sweeps.
	//  1. known in advance to be non-cooperative,
	//  2. fails cosigning during an attempt,
	//  3. co-signs successfully.
	var preimages = []lntypes.Preimage{
		{1}, {2}, {3},
		{4}, {5}, {6},
		{7}, {8}, {9},
	}

	// Create muSig2SignSweep. It fails all the sweeps, works only one time
	// for swapHashes[2] and works any number of times for 5 and 8. This
	// emulates client disconnect after first successful co-signing.
	swapHash2Used := false
	muSig2SignSweep := func(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
		prevoutMap map[wire.OutPoint]*wire.TxOut) (
		[]byte, []byte, error) {

		switch {
		case swapHash == preimages[2].Hash():
			if swapHash2Used {
				return nil, nil, fmt.Errorf("disconnected")
			} else {
				swapHash2Used = true

				return nil, nil, nil
			}

		case swapHash == preimages[5].Hash():
			return nil, nil, nil

		case swapHash == preimages[8].Hash():
			return nil, nil, nil

		default:
			return nil, nil, fmt.Errorf("test error")
		}
	}

	// The first sweep in a group is known in advance to be
	// non-cooperative.
	nonCoopHints := []bool{
		true, false, false,
		true, false, false,
		true, false, false,
	}

	// Expect mockSigner.SignOutputRaw call to sign non-cooperative
	// sweeps.
	expectSignOutputRawChannel := true

	// Two non-cooperative sweeps, one cooperative.
	wantWitnessSizes := []int{4, 4, 4, 4, 4, 1, 4, 4, 1}

	// Expected weight.
	wantWeight := lntypes.WeightUnit(3377)

	testWithMixedBatchCustom(t, store, batcherStore, preimages,
		muSig2SignSweep, nonCoopHints, expectSignOutputRawChannel,
		wantWeight, wantWitnessSizes)
}

// testWithMixedBatchCoopOnly tests mixed batches construction,
// All sweeps are added at once. All the sweeps are cooperative.
func testWithMixedBatchCoopOnly(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	// Create 3 sweeps, all cooperative.
	var preimages = []lntypes.Preimage{
		{1}, {2}, {3},
	}

	// Create muSig2SignSweep, working for all sweeps.
	muSig2SignSweep := func(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
		prevoutMap map[wire.OutPoint]*wire.TxOut) (
		[]byte, []byte, error) {

		return nil, nil, nil
	}

	// All the sweeps are cooperative.
	nonCoopHints := []bool{false, false, false}

	// Do not expect a mockSigner.SignOutputRaw call, because there are no
	// non-cooperative sweeps.
	expectSignOutputRawChannel := false

	// Two non-cooperative sweeps, one cooperative.
	wantWitnessSizes := []int{1, 1, 1}

	// Expected weight.
	wantWeight := lntypes.WeightUnit(856)

	testWithMixedBatchCustom(t, store, batcherStore, preimages,
		muSig2SignSweep, nonCoopHints, expectSignOutputRawChannel,
		wantWeight, wantWitnessSizes)
}

// testWithMixedBatchNonCoopHintOnly tests mixed batches construction,
// All sweeps are added at once. All the sweeps are known to be non-cooperative
// in advance.
func testWithMixedBatchNonCoopHintOnly(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	// Create 3 sweeps, all known to be non-cooperative in advance.
	var preimages = []lntypes.Preimage{
		{1}, {2}, {3},
	}

	// Create muSig2SignSweep, panicking for all sweeps.
	muSig2SignSweep := func(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
		prevoutMap map[wire.OutPoint]*wire.TxOut) (
		[]byte, []byte, error) {

		panic("must not be called in this test")
	}

	// All the sweeps are non-cooperative, this is known in advance.
	nonCoopHints := []bool{true, true, true}

	// Expect mockSigner.SignOutputRaw call to sign non-cooperative
	// sweeps.
	expectSignOutputRawChannel := true

	// Two non-cooperative sweeps, one cooperative.
	wantWitnessSizes := []int{4, 4, 4}

	// Expected weight.
	wantWeight := lntypes.WeightUnit(1345)

	testWithMixedBatchCustom(t, store, batcherStore, preimages,
		muSig2SignSweep, nonCoopHints, expectSignOutputRawChannel,
		wantWeight, wantWitnessSizes)
}

// testWithMixedBatchCoopFailedOnly tests mixed batches construction,
// All sweeps are added at once. All the sweeps fail co-signing.
func testWithMixedBatchCoopFailedOnly(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	// Create 3 sweeps, all fail co-signing.
	var preimages = []lntypes.Preimage{
		{1}, {2}, {3},
	}

	// Create muSig2SignSweep, failing any co-sign attempt.
	muSig2SignSweep := func(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
		prevoutMap map[wire.OutPoint]*wire.TxOut) (
		[]byte, []byte, error) {

		return nil, nil, fmt.Errorf("test error")
	}

	// All the sweeps are non-cooperative, but this is not known in advance.
	nonCoopHints := []bool{false, false, false}

	// Expect mockSigner.SignOutputRaw call to sign non-cooperative
	// sweeps.
	expectSignOutputRawChannel := true

	// Two non-cooperative sweeps, one cooperative.
	wantWitnessSizes := []int{4, 4, 4}

	// Expected weight.
	wantWeight := lntypes.WeightUnit(1345)

	testWithMixedBatchCustom(t, store, batcherStore, preimages,
		muSig2SignSweep, nonCoopHints, expectSignOutputRawChannel,
		wantWeight, wantWitnessSizes)
}

// testFeeRateGrows tests that fee rate of a batch does not decrease and is at
// least as high as the highest fee rate of sweeps.
func testFeeRateGrows(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	// Create a map to store fee rates.
	swap2feeRate := map[lntypes.Hash]chainfee.SatPerKWeight{}
	var swap2feeRateMu sync.Mutex
	setFeeRate := func(swapHash lntypes.Hash, rate chainfee.SatPerKWeight) {
		swap2feeRateMu.Lock()
		defer swap2feeRateMu.Unlock()

		swap2feeRate[swapHash] = rate
	}

	customFeeRate := func(ctx context.Context,
		swapHash lntypes.Hash) (chainfee.SatPerKWeight, error) {

		swap2feeRateMu.Lock()
		defer swap2feeRateMu.Unlock()

		return swap2feeRate[swapHash], nil
	}

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(50_000)
	)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore, WithCustomFeeRate(customFeeRate))

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	setFeeRate(swapHash1, feeRateMedium)
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
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

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{1},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, swapHash1, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Make sure the fee rate is feeRateMedium.
	batch := getOnlyBatch(t, ctx, batcher)
	var (
		numSweeps     int
		cachedFeeRate chainfee.SatPerKWeight
	)
	batch.testRunInEventLoop(ctx, func() {
		numSweeps = len(batch.sweeps)
		cachedFeeRate = batch.rbfCache.FeeRate
	})
	require.Equal(t, 1, numSweeps)
	require.Equal(t, feeRateMedium, cachedFeeRate)

	// Now decrease the fee of sweep1.
	setFeeRate(swapHash1, feeRateLow)
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Tick tock next block.
	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Make sure the fee rate is still feeRateMedium.
	batch.testRunInEventLoop(ctx, func() {
		cachedFeeRate = batch.rbfCache.FeeRate
	})
	require.Equal(t, feeRateMedium, cachedFeeRate)

	// Add sweep2, with feeRateMedium.
	swapHash2 := lntypes.Hash{2, 2, 2}
	setFeeRate(swapHash2, feeRateMedium)
	sweepReq2 := SweepRequest{
		SwapHash: swapHash2,
		Value:    1_000_000,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 1,
		},
		Notifier: &dummyNotifier,
	}

	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 1_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, swapHash2, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Tick tock next block.
	err = lnd.NotifyHeight(602)
	require.NoError(t, err)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Make sure the fee rate is still feeRateMedium.
	batch.testRunInEventLoop(ctx, func() {
		numSweeps = len(batch.sweeps)
		cachedFeeRate = batch.rbfCache.FeeRate
	})
	require.Equal(t, 2, numSweeps)
	require.Equal(t, feeRateMedium, cachedFeeRate)

	// Now update fee rate of second sweep (which is not primary) to
	// feeRateHigh. Fee rate of sweep 1 is still feeRateLow.
	setFeeRate(swapHash2, feeRateHigh)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Tick tock next block.
	err = lnd.NotifyHeight(603)
	require.NoError(t, err)

	// Wait for tx to be published.
	<-lnd.TxPublishChannel

	// Make sure the fee rate increased to feeRateHigh.
	batch.testRunInEventLoop(ctx, func() {
		cachedFeeRate = batch.rbfCache.FeeRate
	})
	require.Equal(t, feeRateHigh, cachedFeeRate)
}

// mockCpfpHelper implements CpfpHelper interface and stores arguments passed
// in its methods to validate correctness of function publishWithCPFP.
type mockCpfpHelper struct {
	// onlineOutpoints specifies which outpoints are capable of
	// participating in presigning.
	onlineOutpoints map[wire.OutPoint]bool

	// presignedBatches is the collection of presigned batches.
	presignedBatches []*wire.MsgTx

	// mu should be hold by all the public methods of this type.
	mu sync.Mutex

	// cleanupCalled is a channel where an element is sent every time
	// CleanupTransactions is called.
	cleanupCalled chan struct{}
}

// newMockCpfpHelper returns new instance of mockCpfpHelper.
func newMockCpfpHelper() *mockCpfpHelper {
	return &mockCpfpHelper{
		onlineOutpoints: make(map[wire.OutPoint]bool),
		cleanupCalled:   make(chan struct{}),
	}
}

// SetOutpointOnline changes the online status of an outpoint.
func (h *mockCpfpHelper) SetOutpointOnline(op wire.OutPoint, online bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.onlineOutpoints[op] = online
}

// findOfflineInputs returns inputs of a tx which are offline.
func (h *mockCpfpHelper) findOfflineInputs(tx *wire.MsgTx) []wire.OutPoint {
	offline := make([]wire.OutPoint, 0, len(tx.TxIn))
	for _, txIn := range tx.TxIn {
		if !h.onlineOutpoints[txIn.PreviousOutPoint] {
			offline = append(offline, txIn.PreviousOutPoint)
		}
	}

	return offline
}

// sign signs the transaction.
func (h *mockCpfpHelper) sign(tx *wire.MsgTx) {
	// Sign all the inputs.
	for i := range tx.TxIn {
		tx.TxIn[i].Witness = wire.TxWitness{
			make([]byte, 64),
		}
	}
}

// getTxFeerate returns fee rate of a transaction.
func (h *mockCpfpHelper) getTxFeerate(tx *wire.MsgTx,
	inputAmt btcutil.Amount) chainfee.SatPerKWeight {

	// "Sign" tx's copy to assess the weight.
	tx2 := tx.Copy()
	h.sign(tx2)
	weight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx2)),
	)
	fee := btcutil.Amount(tx.TxOut[0].Value) - inputAmt

	return chainfee.NewSatPerKWeight(fee, weight)
}

// IsCpfpApplied returns if the input was previously used in any call to the
// SetOutpointOnline method.
func (h *mockCpfpHelper) IsCpfpApplied(ctx context.Context,
	input wire.OutPoint) (bool, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	_, has := h.onlineOutpoints[input]

	return has, nil
}

// Presign tries to presign the transaction. It succeeds if all the inputs
// are online. In case of success it adds the transaction to presignedBatches.
func (h *mockCpfpHelper) Presign(ctx context.Context, tx *wire.MsgTx,
	inputAmt btcutil.Amount) error {

	h.mu.Lock()
	defer h.mu.Unlock()

	if offline := h.findOfflineInputs(tx); len(offline) != 0 {
		return fmt.Errorf("some inputs of tx are offline: %v", offline)
	}

	tx = tx.Copy()
	h.sign(tx)
	h.presignedBatches = append(h.presignedBatches, tx)

	return nil
}

// DestPkScript returns destination pkScript used in 1:1 presigned tx.
func (h *mockCpfpHelper) DestPkScript(ctx context.Context,
	inputs []wire.OutPoint) ([]byte, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	inputsSet := make(map[wire.OutPoint]struct{}, len(inputs))
	for _, input := range inputs {
		inputsSet[input] = struct{}{}
	}
	if len(inputsSet) != len(inputs) {
		return nil, fmt.Errorf("duplicate inputs")
	}

	inputsMatch := func(tx *wire.MsgTx) bool {
		if len(tx.TxIn) != len(inputsSet) {
			return false
		}

		for _, txIn := range tx.TxIn {
			if _, has := inputsSet[txIn.PreviousOutPoint]; !has {
				return false
			}
		}

		return true
	}

	for _, tx := range h.presignedBatches {
		if inputsMatch(tx) {
			return tx.TxOut[0].PkScript, nil
		}
	}

	return nil, fmt.Errorf("tx sweeping inputs %v not found", inputs)
}

// SignTx tries to sign the transaction. If all the inputs are online, it signs
// the exact transaction passed and adds it to presignedBatches. Otherwise it
// looks for a transaction in presignedBatches satisfying the criteria.
func (h *mockCpfpHelper) SignTx(ctx context.Context, tx *wire.MsgTx,
	inputAmt btcutil.Amount,
	minRelayFee chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	// If all the inputs are online, sign this exact transaction.
	if offline := h.findOfflineInputs(tx); len(offline) == 0 {
		tx = tx.Copy()
		h.sign(tx)

		// Add to the collection.
		h.presignedBatches = append(h.presignedBatches, tx)

		return tx, nil
	}

	// Find feerate of input tx.
	inputFeeRate := h.getTxFeerate(tx, inputAmt)

	// Try to find a transaction in the collection satisfying all the
	// criteria of CpfpHelper.SignTx. If there are many such transactions,
	// select a transaction with feerate which is the closest to the feerate
	// of the input tx.
	var (
		bestTx              *wire.MsgTx
		bestFeerateDistance chainfee.SatPerKWeight
	)
	for _, candidate := range h.presignedBatches {
		err := CheckSignedTx(tx, candidate, inputAmt, minRelayFee)
		if err != nil {
			continue
		}

		feeRate := h.getTxFeerate(candidate, inputAmt)
		feeRateDistance := feeRate - inputFeeRate
		if feeRateDistance < 0 {
			feeRateDistance = -feeRateDistance
		}

		if bestTx == nil || feeRateDistance < bestFeerateDistance {
			bestTx = candidate
			bestFeerateDistance = feeRateDistance
		}
	}

	if bestTx == nil {
		return nil, fmt.Errorf("no such presigned tx found")
	}

	return bestTx.Copy(), nil
}

// LoadTx tries to load the transaction by txid. It scans presignedBatches.
func (h *mockCpfpHelper) LoadTx(ctx context.Context,
	txid chainhash.Hash) (*wire.MsgTx, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, tx := range h.presignedBatches {
		if tx.TxHash() == txid {
			return tx.Copy(), nil
		}
	}

	return nil, fmt.Errorf("tx with ID %v not found", txid)
}

// CleanupTransactions removes all transactions related to any of the outpoints.
func (h *mockCpfpHelper) CleanupTransactions(ctx context.Context,
	inputs []wire.OutPoint) error {

	h.mu.Lock()
	defer h.mu.Unlock()

	inputsSet := make(map[wire.OutPoint]struct{}, len(inputs))
	for _, input := range inputs {
		inputsSet[input] = struct{}{}
	}
	if len(inputsSet) != len(inputs) {
		return fmt.Errorf("duplicate inputs")
	}

	var presignedBatches []*wire.MsgTx

	// Filter out transactions spending any of the inputs passed.
	for _, tx := range h.presignedBatches {
		var match bool
		for _, txIn := range tx.TxIn {
			if _, has := inputsSet[txIn.PreviousOutPoint]; has {
				match = true
				break
			}
		}

		if !match {
			presignedBatches = append(presignedBatches, tx)
		}
	}

	h.presignedBatches = presignedBatches

	h.cleanupCalled <- struct{}{}

	return nil
}

// dummySweepFetcherMock implements SweepFetcher by returning blank SweepInfo.
// It is used in TestCPFP, because it doesn't use any fields from SweepInfo.
type dummySweepFetcherMock struct {
}

// FetchSweep returns blank SweepInfo.
func (f *dummySweepFetcherMock) FetchSweep(ctx context.Context,
	hash lntypes.Hash) (*SweepInfo, error) {

	return &SweepInfo{
		// Set Timeout to prevent warning messages about timeout=0.
		Timeout: 1000,
	}, nil
}

// testCPFP_input1_offline_then_input2 tests CPFP mode for the following
// scenario: first input is added, then goes offline, then feerate grows, one of
// presigned transactions is published, feerate grows further, then CPFP is used
// and then another online input is added and is assigned to another batch.
func testCPFP_input1_offline_then_input2(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper))

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}

	// This should fail, because the input is offline.
	cpfpHelper.SetOutpointOnline(op1, false)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.Error(t, err)
	require.ErrorContains(t, err, "offline")

	// Enable the input and try again.
	cpfpHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.NoError(t, err)

	// Increase fee rate and turn off the input, so it can't sign updated
	// tx. The feerate is close to the feerate of one of presigned txs, so
	// there is no CPFP.
	setFeeRate(feeRateMedium)
	cpfpHelper.SetOutpointOnline(op1, false)

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	parent := <-lnd.TxPublishChannel
	require.Len(t, parent.TxIn, 1)
	require.Len(t, parent.TxOut, 1)
	require.Equal(t, op1, parent.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(987034), parent.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent.TxOut[0].PkScript)

	// Make sure the fee rate is feeRateMedium.
	batch := getOnlyBatch(t, ctx, batcher)
	var (
		numSweeps     int
		cachedFeeRate chainfee.SatPerKWeight
	)
	batch.testRunInEventLoop(ctx, func() {
		numSweeps = len(batch.sweeps)
		cachedFeeRate = batch.rbfCache.FeeRate
	})
	require.Equal(t, 1, numSweeps)
	require.Equal(t, feeRateMedium, cachedFeeRate)

	// Raise feerate and trigger new publishing. The parent tx should be the
	// same plus a CPFP tx.
	setFeeRate(feeRateHigh)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(601))

	parent2 := <-lnd.TxPublishChannel
	child := <-lnd.TxPublishChannel
	require.Equal(t, parent.TxHash(), parent2.TxHash())
	require.Len(t, child.TxIn, 1)
	require.Len(t, child.TxOut, 1)
	parentOp := wire.OutPoint{
		Hash:  parent2.TxHash(),
		Index: 0,
	}
	require.Equal(t, parentOp, child.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(966600), child.TxOut[0].Value)
	require.Equal(t, batchPkScript, child.TxOut[0].PkScript)

	// Now add another input. It is online, but the first input is still
	// offline, so another input should go to another batch.
	swapHash2 := lntypes.Hash{2, 2, 2}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2},
		Index: 2,
	}
	sweepReq2 := SweepRequest{
		SwapHash: swapHash2,
		Value:    2_000_000,
		Outpoint: op2,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op2, true)
	err = batcher.PresignSweep(ctx, op2, 2_000_000, destAddr)
	require.NoError(t, err)

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	batch2 := <-lnd.TxPublishChannel
	require.Len(t, batch2.TxIn, 1)
	require.Len(t, batch2.TxOut, 1)
	require.Equal(t, op2, batch2.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(1984160), batch2.TxOut[0].Value)
	require.Equal(t, batchPkScript, batch2.TxOut[0].PkScript)

	// Now confirm the first batch. Make sure its presigned transactions
	// were removed, but not the transactions of the second batch.
	presignedSize1 := len(cpfpHelper.presignedBatches)

	parent2hash := parent2.TxHash()
	spendDetail := &chainntnfs.SpendDetail{
		SpentOutPoint:     &sweepReq1.Outpoint,
		SpendingTx:        parent2,
		SpenderTxHash:     &parent2hash,
		SpenderInputIndex: 0,
		SpendingHeight:    601,
	}
	lnd.SpendChannel <- spendDetail
	<-lnd.RegisterConfChannel
	require.NoError(t, lnd.NotifyHeight(604))
	lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: parent2,
	}

	<-cpfpHelper.cleanupCalled

	presignedSize2 := len(cpfpHelper.presignedBatches)
	require.Greater(t, presignedSize2, 0)
	require.Greater(t, presignedSize1, presignedSize2)

	// Make sure we still have presigned transactions for the second batch.
	cpfpHelper.SetOutpointOnline(op2, false)
	_, err = cpfpHelper.SignTx(
		ctx, batch2, 2_000_000, chainfee.FeePerKwFloor,
	)
	require.NoError(t, err)
}

// testCPFP_two_inputs_one_goes_offline tests CPFP mode for the following
// scenario: two online inputs are added, then one of them goes offline, then
// feerate grows and a presigned transaction is used.
func testCPFP_two_inputs_one_goes_offline(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Add second sweep.
	swapHash2 := lntypes.Hash{2, 2, 2}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2},
		Index: 2,
	}
	sweepReq2 := SweepRequest{
		SwapHash: swapHash2,
		Value:    2_000_000,
		Outpoint: op2,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op2, true)
	err = batcher.PresignSweep(ctx, op2, 2_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Wait for a transactions to be published.
	parent := <-lnd.TxPublishChannel
	require.Len(t, parent.TxIn, 2)
	require.Len(t, parent.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2},
		[]wire.OutPoint{
			parent.TxIn[0].PreviousOutPoint,
			parent.TxIn[1].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(2993740), parent.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent.TxOut[0].PkScript)

	// Now turn off the second input, raise feerate and trigger new
	// publishing. The feerate is close to one of the presigned feerates,
	// so this should result in RBF.
	cpfpHelper.SetOutpointOnline(op2, false)
	setFeeRate(feeRateMedium)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, batcher.AddSweep(&sweepReq2))
	require.NoError(t, lnd.NotifyHeight(601))

	parent2 := <-lnd.TxPublishChannel
	require.NotEqual(t, parent.TxHash(), parent2.TxHash())
	require.Len(t, parent2.TxIn, 2)
	require.Len(t, parent2.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2},
		[]wire.OutPoint{
			parent.TxIn[0].PreviousOutPoint,
			parent.TxIn[1].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(2979503), parent2.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent2.TxOut[0].PkScript)
}

// testCPFP_cpfp_previous_version tests CPFP mode for the following scenario:
// one input is added, a transaction is published, then the input goes offline
// and feerate grows, RBF is attempted, but broadcast fails, so the batcher
// CPFPs previously published version.
func testCPFP_cpfp_previous_version(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	parent := <-lnd.TxPublishChannel
	require.Len(t, parent.TxIn, 1)
	require.Len(t, parent.TxOut, 1)
	require.Equal(t, op1, parent.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(996040), parent.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent.TxOut[0].PkScript)

	// Now turn off the first input, raise feerate and trigger new
	// publishing, which will fail.
	var failedToPublishTx *wire.MsgTx
	lnd.PublishHandler = func(ctx context.Context, tx *wire.MsgTx,
		label string) error {

		// We should fail the first publishing, which is a sweep,
		// but we shouldn't fail CPFP publishing.
		if strings.HasPrefix(label, cpfpLabelPrefix) {
			return nil
		}

		failedToPublishTx = tx

		return fmt.Errorf("test error")
	}
	cpfpHelper.SetOutpointOnline(op1, false)
	setFeeRate(feeRateMedium)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(601))

	child := <-lnd.TxPublishChannel
	require.NotEqual(t, parent.TxHash(), child.TxHash())
	require.Len(t, child.TxIn, 1)
	require.Len(t, child.TxOut, 1)
	require.Equal(t, wire.OutPoint{
		Hash:  parent.TxHash(),
		Index: 0,
	}, child.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(974950), child.TxOut[0].Value)

	// Make sure the failed attempt used higher feerate than parent.
	require.Equal(t, int64(987034), failedToPublishTx.TxOut[0].Value)
}

// testCPFP_no_cpfp_if_all_online tests CPFP mode for the following scenario:
// one input is added, a transaction is published, then feerate grows, RBF is
// attempted, but broadcast fails, but CPFP is not used, because all the inputs
// are online (which is deduced by SignTx signing a tx with the same feerate as
// requested).
func testCPFP_no_cpfp_if_all_online(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	parent := <-lnd.TxPublishChannel
	require.Len(t, parent.TxIn, 1)
	require.Len(t, parent.TxOut, 1)
	require.Equal(t, op1, parent.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(996040), parent.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent.TxOut[0].PkScript)

	// Replace the logger in the batch with wrappedLogger to watch messages.
	batch := getOnlyBatch(t, ctx, batcher)
	testLogger := &wrappedLogger{
		Logger: batch.log(),
	}
	batch.setLog(testLogger)

	// Now turn off the first input, raise feerate and trigger new
	// publishing, which will fail.
	lnd.PublishHandler = func(ctx context.Context, tx *wire.MsgTx,
		label string) error {

		return fmt.Errorf("test error")
	}
	setFeeRate(feeRateMedium)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(601))

	// Wait for batcher to log that CPFP is not needed.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		testLogger.mu.Lock()
		defer testLogger.mu.Unlock()

		assert.Contains(
			c, testLogger.infoMessages, "CPFP is not needed",
		)
	}, test.Timeout, eventuallyCheckFrequency)
}

// testCPFP_first_publish_fails tests CPFP mode for the following scenario:
// one input is added and goes offline, feerate grows a transaction is attempted
// to be published, but fails, no CPFP is attempted. Then the input goes online
// and is published being signed online.
func testCPFP_first_publish_fails(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.NoError(t, err)
	cpfpHelper.SetOutpointOnline(op1, false)

	// Make sure that publish attempt fails.
	lnd.PublishHandler = func(ctx context.Context, tx *wire.MsgTx,
		label string) error {

		return fmt.Errorf("test error")
	}

	// Add the sweep, triggering the publish attempt.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Replace the logger in the batch with wrappedLogger to watch messages.
	batch := getOnlyBatch(t, ctx, batcher)
	testLogger := &wrappedLogger{
		Logger: batch.log(),
	}
	batch.setLog(testLogger)

	// Trigger another publish attempt in case "CPFP is not needed" was
	// logged before we installed the logger watcher.
	require.NoError(t, lnd.NotifyHeight(601))

	// Wait for batcher to log that CPFP is not needed.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		testLogger.mu.Lock()
		defer testLogger.mu.Unlock()

		assert.Contains(
			c, testLogger.infoMessages, "CPFP is not needed",
		)
	}, test.Timeout, eventuallyCheckFrequency)

	// Now turn on the first input, raise feerate and trigger new
	// publishing, which should succeed.
	lnd.PublishHandler = nil
	setFeeRate(feeRateMedium)
	cpfpHelper.SetOutpointOnline(op1, true)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(602))

	// Wait for a transactions to be published.
	parent := <-lnd.TxPublishChannel
	require.Len(t, parent.TxIn, 1)
	require.Len(t, parent.TxOut, 1)
	require.Equal(t, op1, parent.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(988120), parent.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent.TxOut[0].PkScript)
}

// testCPFP_cpfp_publishing_fails tests CPFP mode for the following scenario:
// one input is added, a transaction is published, then the input goes offline
// and feerate grows, RBF is published and then CPFP is attempted to achieve
// the exact desired fee rate, but fails to be published. After then another
// block comes in and both the parent and the child are published and this
// succeeds.
func testCPFP_cpfp_publishing_fails(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	// Create the first sweep.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweep(ctx, op1, 1_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	parent := <-lnd.TxPublishChannel
	require.Len(t, parent.TxIn, 1)
	require.Len(t, parent.TxOut, 1)
	require.Equal(t, op1, parent.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(996040), parent.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent.TxOut[0].PkScript)

	// Replace the logger in the batch with wrappedLogger to watch messages.
	batch := getOnlyBatch(t, ctx, batcher)
	testLogger := &wrappedLogger{
		Logger: batch.log(),
	}
	batch.setLog(testLogger)

	// Now turn off the first input, raise feerate and trigger new
	// publishing, which will succeed. But then the CPFP will fail.
	var failedToPublishTx *wire.MsgTx
	lnd.PublishHandler = func(ctx context.Context, tx *wire.MsgTx,
		label string) error {

		// We should fail the CPFP only.
		if strings.HasPrefix(label, cpfpLabelPrefix) {
			failedToPublishTx = tx

			return fmt.Errorf("test error")
		}

		return nil
	}
	cpfpHelper.SetOutpointOnline(op1, false)
	setFeeRate(feeRateHigh)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(601))

	// Expect new version of the batch to be published. This is one
	// of the presigned transactions.
	parent2 := <-lnd.TxPublishChannel
	require.NotEqual(t, parent.TxHash(), parent2.TxHash())
	require.Len(t, parent2.TxIn, 1)
	require.Len(t, parent2.TxOut, 1)
	require.Equal(t, op1, parent2.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(987034), parent2.TxOut[0].Value)
	require.Equal(t, batchPkScript, parent2.TxOut[0].PkScript)

	// Wait for batcher to log that CPFP has failed.
	require.Eventually(t, func() bool {
		testLogger.mu.Lock()
		defer testLogger.mu.Unlock()

		for _, msg := range testLogger.infoMessages {
			match := strings.Contains(
				msg, "failed to publish child tx",
			)
			if match {
				return true
			}
		}

		return false
	}, test.Timeout, eventuallyCheckFrequency)

	// Make sure that the failed to publish tx is our expected CPFP
	// spending parent2.
	require.Len(t, failedToPublishTx.TxIn, 1)
	require.Len(t, failedToPublishTx.TxOut, 1)
	require.Equal(t, wire.OutPoint{
		Hash:  parent2.TxHash(),
		Index: 0,
	}, failedToPublishTx.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(966600), failedToPublishTx.TxOut[0].Value)
	require.Equal(t, batchPkScript, failedToPublishTx.TxOut[0].PkScript)

	// Great, now les all published transactions pass and trigger another
	// publishing.
	lnd.PublishHandler = nil
	require.NoError(t, lnd.NotifyHeight(602))

	// Expect a parent and a child to be published.
	parent2a := <-lnd.TxPublishChannel
	require.Equal(t, parent2.TxHash(), parent2a.TxHash())

	child := <-lnd.TxPublishChannel
	require.Len(t, child.TxIn, 1)
	require.Len(t, child.TxOut, 1)
	require.Equal(t, wire.OutPoint{
		Hash:  parent2a.TxHash(),
		Index: 0,
	}, child.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(966600), child.TxOut[0].Value)
	require.Equal(t, batchPkScript, child.TxOut[0].PkScript)
}

// testCPFP_cpfp_and_regular_sweeps tests a combination of CPFP mode and regular
// mode for the following scenario: one regular input is added, then a CPFP
// input is added and it goes to another batch, because they shouldn't appear
// in the same batch. Then another regular and another CPFP inputs are added and
// go to the existing batches of their types.
func testCPFP_cpfp_and_regular_sweeps(t *testing.T, store testStore,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow    = chainfee.SatPerKWeight(10_000)
		feeRateMedium = chainfee.SatPerKWeight(30_000)
		feeRateHigh   = chainfee.SatPerKWeight(40_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	cpfpHelper := newMockCpfpHelper()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore,
		WithCustomFeeRate(customFeeRate), WithCpfpHelper(cpfpHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	setFeeRate(feeRateLow)

	/////////////////////////////////////
	// Create the first regular sweep. //
	/////////////////////////////////////
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	sweepReq1 := SweepRequest{
		SwapHash: swapHash1,
		Value:    1_000_000,
		Outpoint: op1,
		Notifier: &dummyNotifier,
	}

	swap1 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 1_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{1},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, swapHash1, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	tx1 := <-lnd.TxPublishChannel
	require.Len(t, tx1.TxIn, 1)
	require.Len(t, tx1.TxOut, 1)

	//////////////////////////////////
	// Create the first CPFP sweep. //
	//////////////////////////////////
	swapHash2 := lntypes.Hash{2, 2, 2}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2},
		Index: 2,
	}

	swap2 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 2_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{2},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, swapHash2, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	sweepReq2 := SweepRequest{
		SwapHash: swapHash2,
		Value:    2_000_000,
		Outpoint: op2,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op2, true)
	err = batcher.PresignSweep(ctx, op2, 2_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	tx2 := <-lnd.TxPublishChannel
	require.Len(t, tx2.TxIn, 1)
	require.Len(t, tx2.TxOut, 1)
	require.Equal(t, op2, tx2.TxIn[0].PreviousOutPoint)

	//////////////////////////////////////
	// Create the second regular sweep. //
	//////////////////////////////////////
	swapHash3 := lntypes.Hash{3, 3, 3}
	op3 := wire.OutPoint{
		Hash:  chainhash.Hash{3, 3},
		Index: 3,
	}
	sweepReq3 := SweepRequest{
		SwapHash: swapHash3,
		Value:    4_000_000,
		Outpoint: op3,
		Notifier: &dummyNotifier,
	}

	swap3 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 4_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{3},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, swapHash3, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq3))

	///////////////////////////////////
	// Create the second CPFP sweep. //
	///////////////////////////////////
	swapHash4 := lntypes.Hash{4, 4, 4}
	op4 := wire.OutPoint{
		Hash:  chainhash.Hash{4, 4},
		Index: 4,
	}

	swap4 := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			CltvExpiry:      111,
			AmountRequested: 3_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,

			// Make preimage unique to pass SQL constraints.
			Preimage: lntypes.Preimage{4},
		},

		DestAddr:        destAddr,
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, swapHash4, swap4)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	sweepReq4 := SweepRequest{
		SwapHash: swapHash4,
		Value:    3_000_000,
		Outpoint: op4,
		Notifier: &dummyNotifier,
	}
	cpfpHelper.SetOutpointOnline(op4, true)
	err = batcher.PresignSweep(ctx, op4, 4_000_000, destAddr)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq4))

	// Wait for the both batches to have two sweeps.
	require.Eventually(t, func() bool {
		// Make sure there are two batches.
		batches := getBatches(ctx, batcher)
		if len(batches) != 2 {
			return false
		}

		// Make sure each batch has two sweeps.
		for _, batch := range batches {
			var numSweeps int
			batch.testRunInEventLoop(ctx, func() {
				numSweeps = len(batch.sweeps)
			})
			if numSweeps != 2 {
				return false
			}
		}

		return true
	}, test.Timeout, eventuallyCheckFrequency)

	// Mine a block to trigger both batches publishing.
	require.NoError(t, lnd.NotifyHeight(601))

	// Wait for a transactions to be published.
	tx3 := <-lnd.TxPublishChannel
	require.Len(t, tx3.TxIn, 2)
	require.Len(t, tx3.TxOut, 1)
	require.Equal(t, int64(4993740), tx3.TxOut[0].Value)

	tx4 := <-lnd.TxPublishChannel
	require.Len(t, tx4.TxIn, 2)
	require.Len(t, tx4.TxOut, 1)
	require.Equal(t, int64(4993740), tx4.TxOut[0].Value)
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

// TestTxLabeler tests transaction labels.
func TestTxLabeler(t *testing.T) {
	runTests(t, testTxLabeler)
}

// TestPublishErrorHandler tests transaction labels.
func TestPublishErrorHandler(t *testing.T) {
	runTests(t, testPublishErrorHandler)
}

// TestSweepBatcherSimpleLifecycle tests the simple lifecycle of the batches
// that are created and run by the batcher.
func TestSweepBatcherSimpleLifecycle(t *testing.T) {
	runTests(t, testSweepBatcherSimpleLifecycle)
}

// TestDelays tests that WithInitialDelay and WithPublishDelay work.
func TestDelays(t *testing.T) {
	runTests(t, testDelays)
}

// TestMaxSweepsPerBatch tests the limit on max number of sweeps per batch.
func TestMaxSweepsPerBatch(t *testing.T) {
	runTests(t, testMaxSweepsPerBatch)
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

// TestWithMixedBatch tests mixed batches construction. It also tests
// non-cooperative sweeping (using a preimage). Sweeps are added one by one.
func TestWithMixedBatch(t *testing.T) {
	runTests(t, testWithMixedBatch)
}

// TestWithMixedBatchLarge tests mixed batches construction, many sweeps.
// All sweeps are added at once.
func TestWithMixedBatchLarge(t *testing.T) {
	runTests(t, testWithMixedBatchLarge)
}

// TestWithMixedBatchCoopOnly tests mixed batches construction,
// All sweeps are added at once. All the sweeps are cooperative.
func TestWithMixedBatchCoopOnly(t *testing.T) {
	runTests(t, testWithMixedBatchCoopOnly)
}

// TestWithMixedBatchNonCoopHintOnly tests mixed batches construction,
// All sweeps are added at once. All the sweeps are known to be non-cooperative
// in advance.
func TestWithMixedBatchNonCoopHintOnly(t *testing.T) {
	runTests(t, testWithMixedBatchNonCoopHintOnly)
}

// TestWithMixedBatchCoopFailedOnly tests mixed batches construction,
// All sweeps are added at once. All the sweeps fail co-signing.
func TestWithMixedBatchCoopFailedOnly(t *testing.T) {
	runTests(t, testWithMixedBatchCoopFailedOnly)
}

// TestFeeRateGrows tests that fee rate of a batch does not decrease and is at
// least as high as the highest fee rate of sweeps.
func TestFeeRateGrows(t *testing.T) {
	runTests(t, testFeeRateGrows)
}

// TestCPFP tests CPFP mode. This test doesn't use loopdb.
func TestCPFP(t *testing.T) {
	logger := btclog.NewBackend(os.Stdout).Logger("SWEEP")
	logger.SetLevel(btclog.LevelTrace)
	UseLogger(logger)

	t.Run("input1_offline_then_input2", func(t *testing.T) {
		testCPFP_input1_offline_then_input2(t, NewStoreMock())
	})

	t.Run("two_inputs_one_goes_offline", func(t *testing.T) {
		testCPFP_two_inputs_one_goes_offline(t, NewStoreMock())
	})

	t.Run("cpfp_previous_version", func(t *testing.T) {
		testCPFP_cpfp_previous_version(t, NewStoreMock())
	})

	t.Run("no_cpfp_if_all_online", func(t *testing.T) {
		testCPFP_no_cpfp_if_all_online(t, NewStoreMock())
	})

	t.Run("first_publish_fails", func(t *testing.T) {
		testCPFP_first_publish_fails(t, NewStoreMock())
	})

	t.Run("cpfp_publishing_fails", func(t *testing.T) {
		testCPFP_cpfp_publishing_fails(t, NewStoreMock())
	})

	t.Run("cpfp_and_regular_sweeps", func(t *testing.T) {
		runTests(t, testCPFP_cpfp_and_regular_sweeps)
	})
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

	mu sync.Mutex
}

// UpsertSweep inserts a sweep into the database, or updates an existing sweep
// if it already exists. This wrapper was added to update sweepsSet.
func (s *loopdbBatcherStore) UpsertSweep(ctx context.Context,
	sweep *dbSweep) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.BatcherStore.UpsertSweep(ctx, sweep)
	if err == nil {
		s.sweepsSet[sweep.SwapHash] = struct{}{}
	}
	return err
}

// AssertSweepStored asserts that a sweep is stored.
func (s *loopdbBatcherStore) AssertSweepStored(id lntypes.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

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
