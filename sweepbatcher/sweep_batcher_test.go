package sweepbatcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntypes"
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

func testMuSig2SignSweep(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
	prevoutMap map[wire.OutPoint]*wire.TxOut) (
	[]byte, []byte, error) {

	return nil, nil, nil
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

// TestSweepBatcherBatchCreation tests that sweep requests enter the expected
// batch based on their timeout distance.
func TestSweepBatcherBatchCreation(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := loopdb.NewStoreMock(t)

	batcherStore := NewStoreMock()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, nil, lnd.ChainParams, batcherStore, store)
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
		},

		SwapInvoice: swapInvoice,
	}

	err := store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	batcher.sweepReqs <- sweepReq1

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Insert the same swap twice, this should be a noop.
	batcher.sweepReqs <- sweepReq1

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

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
		},
		SwapInvoice: swapInvoice,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	batcher.sweepReqs <- sweepReq2

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
		},
		SwapInvoice: swapInvoice,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	batcher.sweepReqs <- sweepReq3

	// Batcher should create a second batch as timeout distance is greater
	// than the threshold
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since the second batch got created we check that it registered its
	// primary sweep's spend.
	<-lnd.RegisterSpendChannel

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps in it.
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

// TestSweepBatcherSimpleLifecycle tests the simple lifecycle of the batches
// that are created and run by the batcher.
func TestSweepBatcherSimpleLifecycle(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := loopdb.NewStoreMock(t)

	batcherStore := NewStoreMock()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, nil, lnd.ChainParams, batcherStore, store)
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
		},
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err := store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	batcher.sweepReqs <- sweepReq1

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

	err = lnd.NotifyHeight(601)
	require.NoError(t, err)

	// After receiving a height notification the batch will step again,
	// leading to a new spend monitoring.
	require.Eventually(t, func() bool {
		return batch.currentHeight == 601
	}, test.Timeout, eventuallyCheckFrequency)

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

// TestSweepBatcherSweepReentry tests that when an old version of the batch tx
// gets confirmed the sweep leftovers are sent back to the batcher.
func TestSweepBatcherSweepReentry(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := loopdb.NewStoreMock(t)

	batcherStore := NewStoreMock()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, nil, lnd.ChainParams, batcherStore, store)
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
		},
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err := store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
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
		},
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
		},
		SwapInvoice:     swapInvoice,
		SweepConfTarget: 111,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Feed the sweeps to the batcher.
	batcher.sweepReqs <- sweepReq1

	// After inserting the primary (first) sweep, a spend monitor should be
	// registered.
	<-lnd.RegisterSpendChannel

	batcher.sweepReqs <- sweepReq2

	batcher.sweepReqs <- sweepReq3

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
				Value:    int64(sweepReq1.Value.ToUnit(btcutil.AmountSatoshi)),
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
		SpendingHeight:    601,
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

// TestSweepBatcherNonWalletAddr tests that sweep requests that sweep to a non
// wallet address enter individual batches.
func TestSweepBatcherNonWalletAddr(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := loopdb.NewStoreMock(t)

	batcherStore := NewStoreMock()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, nil, lnd.ChainParams, batcherStore, store)
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
		},
		IsExternalAddr: true,
		SwapInvoice:    swapInvoice,
	}

	err := store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	batcher.sweepReqs <- sweepReq1

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Insert the same swap twice, this should be a noop.
	batcher.sweepReqs <- sweepReq1

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
		},
		SwapInvoice:    swapInvoice,
		IsExternalAddr: true,
	}

	err = store.CreateLoopOut(ctx, sweepReq2.SwapHash, swap2)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	batcher.sweepReqs <- sweepReq2

	// Batcher should create a second batch as first batch is a non wallet
	// addr batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

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
		},
		SwapInvoice:    swapInvoice,
		IsExternalAddr: true,
	}

	err = store.CreateLoopOut(ctx, sweepReq3.SwapHash, swap3)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	batcher.sweepReqs <- sweepReq3

	// Batcher should create a new batch as timeout distance is greater than
	// the threshold
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	require.Eventually(t, func() bool {
		// Verify that each batch has the correct number of sweeps in it.
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

// TestSweepBatcherComposite tests that sweep requests that sweep to both wallet
// addresses and non-wallet addresses enter the correct batches.
func TestSweepBatcherComposite(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := loopdb.NewStoreMock(t)

	batcherStore := NewStoreMock()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, nil, lnd.ChainParams, batcherStore, store)
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
		},

		SwapInvoice: swapInvoice,
	}

	err := store.CreateLoopOut(ctx, sweepReq1.SwapHash, swap1)
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
		},
		SwapInvoice: swapInvoice,
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
		},
		SwapInvoice:    swapInvoice,
		IsExternalAddr: true,
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
		},
		SwapInvoice: swapInvoice,
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
		},
		SwapInvoice: swapInvoice,
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
		},
		SwapInvoice:    swapInvoice,
		IsExternalAddr: true,
	}

	err = store.CreateLoopOut(ctx, sweepReq6.SwapHash, swap6)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	batcher.sweepReqs <- sweepReq1

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Insert the same swap twice, this should be a noop.
	batcher.sweepReqs <- sweepReq1

	batcher.sweepReqs <- sweepReq2

	// Batcher should not create a second batch as timeout distance is small
	// enough.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, eventuallyCheckFrequency)

	batcher.sweepReqs <- sweepReq3

	// Batcher should create a second batch as this sweep pays to a non
	// wallet address.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	batcher.sweepReqs <- sweepReq4

	// Batcher should create a third batch as timeout distance is greater
	// than the threshold.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	batcher.sweepReqs <- sweepReq5

	// Batcher should not create a fourth batch as timeout distance is small
	// enough for it to join the last batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 3
	}, test.Timeout, eventuallyCheckFrequency)

	batcher.sweepReqs <- sweepReq6

	// Batcher should create a fourth batch as this sweep pays to a non
	// wallet address.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 4
	}, test.Timeout, eventuallyCheckFrequency)

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

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

// TestGetFeePortionForSweep tests that the fee portion for a sweep is correctly
// calculated.
func TestGetFeePortionForSweep(t *testing.T) {
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

// TestRestoringEmptyBatch tests that the batcher can be restored with an empty
// batch.
func TestRestoringEmptyBatch(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(context.Background())

	store := loopdb.NewStoreMock(t)

	batcherStore := NewStoreMock()
	_, err := batcherStore.InsertSweepBatch(ctx, &dbBatch{})
	require.NoError(t, err)

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, nil, lnd.ChainParams, batcherStore, store)

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
		},

		SwapInvoice: swapInvoice,
	}

	err = store.CreateLoopOut(ctx, sweepReq.SwapHash, swap)
	require.NoError(t, err)
	store.AssertLoopOutStored()

	// Deliver sweep request to batcher.
	batcher.sweepReqs <- sweepReq

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

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

	// Now make it quit by canceling the context.
	cancel()
	wg.Wait()

	checkBatcherError(t, runErr)
}
