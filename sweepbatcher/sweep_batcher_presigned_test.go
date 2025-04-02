package sweepbatcher

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockPresignedHelper implements PresignedHelper interface and stores arguments
// passed in its methods to validate correctness of function publishPresigned.
type mockPresignedHelper struct {
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

// newMockPresignedHelper returns new instance of mockPresignedHelper.
func newMockPresignedHelper() *mockPresignedHelper {
	return &mockPresignedHelper{
		onlineOutpoints: make(map[wire.OutPoint]bool),
		cleanupCalled:   make(chan struct{}),
	}
}

// SetOutpointOnline changes the online status of an outpoint.
func (h *mockPresignedHelper) SetOutpointOnline(op wire.OutPoint, online bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.onlineOutpoints[op] = online
}

// offlineInputs returns inputs of a tx which are offline.
func (h *mockPresignedHelper) offlineInputs(tx *wire.MsgTx) []wire.OutPoint {
	offline := make([]wire.OutPoint, 0, len(tx.TxIn))
	for _, txIn := range tx.TxIn {
		if !h.onlineOutpoints[txIn.PreviousOutPoint] {
			offline = append(offline, txIn.PreviousOutPoint)
		}
	}

	return offline
}

// sign signs the transaction.
func (h *mockPresignedHelper) sign(tx *wire.MsgTx) {
	// Sign all the inputs.
	for i := range tx.TxIn {
		tx.TxIn[i].Witness = wire.TxWitness{
			make([]byte, 64),
		}
	}
}

// getTxFeerate returns fee rate of a transaction.
func (h *mockPresignedHelper) getTxFeerate(tx *wire.MsgTx,
	inputAmt btcutil.Amount) chainfee.SatPerKWeight {

	// "Sign" tx's copy to assess the weight.
	tx2 := tx.Copy()
	h.sign(tx2)
	weight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx2)),
	)
	fee := inputAmt - btcutil.Amount(tx.TxOut[0].Value)

	return chainfee.NewSatPerKWeight(fee, weight)
}

// IsPresigned returns if the input was previously used in any call to the
// SetOutpointOnline method.
func (h *mockPresignedHelper) IsPresigned(ctx context.Context,
	input wire.OutPoint) (bool, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	_, has := h.onlineOutpoints[input]

	return has, nil
}

// Presign tries to presign the transaction. It succeeds if all the inputs
// are online. In case of success it adds the transaction to presignedBatches.
func (h *mockPresignedHelper) Presign(ctx context.Context, tx *wire.MsgTx,
	inputAmt btcutil.Amount) error {

	h.mu.Lock()
	defer h.mu.Unlock()

	if offline := h.offlineInputs(tx); len(offline) != 0 {
		return fmt.Errorf("some inputs of tx are offline: %v", offline)
	}

	tx = tx.Copy()
	h.sign(tx)
	h.presignedBatches = append(h.presignedBatches, tx)

	return nil
}

// DestPkScript returns destination pkScript used in 1:1 presigned tx.
func (h *mockPresignedHelper) DestPkScript(ctx context.Context,
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
func (h *mockPresignedHelper) SignTx(ctx context.Context, tx *wire.MsgTx,
	inputAmt btcutil.Amount, minRelayFee, feeRate chainfee.SatPerKWeight,
	presignedOnly bool) (*wire.MsgTx, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	// If all the inputs are online and presignedOnly is not set, sign
	// this exact transaction.
	if offline := h.offlineInputs(tx); len(offline) == 0 && !presignedOnly {
		tx = tx.Copy()
		h.sign(tx)

		// Add to the collection.
		h.presignedBatches = append(h.presignedBatches, tx)

		return tx, nil
	}

	// Try to find a transaction in the collection satisfying all the
	// criteria of PresignedHelper.SignTx. If there are many such
	// transactions, select a transaction with feerate which is the closest
	// to the feerate of the input tx.
	var (
		bestTx              *wire.MsgTx
		bestFeerateDistance chainfee.SatPerKWeight
	)
	for _, candidate := range h.presignedBatches {
		err := CheckSignedTx(tx, candidate, inputAmt, minRelayFee)
		if err != nil {
			continue
		}

		feeRateDistance := h.getTxFeerate(candidate, inputAmt) - feeRate
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

// CleanupTransactions removes all transactions related to any of the outpoints.
func (h *mockPresignedHelper) CleanupTransactions(ctx context.Context,
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

// sweepTimeout is swap timeout block height used in tests of presigned mode.
const sweepTimeout = 1000

// dummySweepFetcherMock implements SweepFetcher by returning blank SweepInfo.
// It is used in TestPresigned, because it doesn't use any fields of SweepInfo.
type dummySweepFetcherMock struct {
}

// FetchSweep returns blank SweepInfo.
func (f *dummySweepFetcherMock) FetchSweep(_ context.Context,
	_ lntypes.Hash, _ wire.OutPoint) (*SweepInfo, error) {

	return &SweepInfo{
		// Set Timeout to prevent warning messages about timeout=0.
		Timeout: sweepTimeout,
	}, nil
}

// testPresigned_input1_offline_then_input2 tests presigned mode for the
// following scenario: first input is added, then goes offline, then feerate
// grows, one of presigned transactions is published, and then another online
// input is added and is assigned to another batch.
func testPresigned_input1_offline_then_input2(t *testing.T,
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
		feeRateHigh   = chainfee.SatPerKWeight(31_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	presignedHelper := newMockPresignedHelper()

	batcher := NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper))

	batcherErrChan := make(chan error)
	go func() {
		batcherErrChan <- batcher.Run(ctx)
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
		Inputs: []Input{{
			Value:    1_000_000,
			Outpoint: op1,
		}},
		Notifier: &dummyNotifier,
	}

	// Make sure that the batcher crashes if AddSweep is called before
	// PresignSweepsGroup even if the input is online.
	presignedHelper.SetOutpointOnline(op1, true)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	err = <-batcherErrChan
	require.Error(t, err)
	require.ErrorContains(t, err, "was not accepted by new batch")

	// Start the batcher again.
	batcher = NewBatcher(lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper))
	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// This should fail, because the input is offline.
	presignedHelper.SetOutpointOnline(op1, false)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op1, Value: 1_000_000}},
		sweepTimeout, destAddr,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "offline")

	// Enable the input and try again.
	presignedHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op1, Value: 1_000_000}},
		sweepTimeout, destAddr,
	)
	require.NoError(t, err)

	// Increase fee rate and turn off the input, so it can't sign updated
	// tx. The feerate is close to the feerate of one of presigned txs.
	setFeeRate(feeRateMedium)
	presignedHelper.SetOutpointOnline(op1, false)

	// Deliver sweep request to batcher.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	tx := <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 1)
	require.Len(t, tx.TxOut, 1)
	require.Equal(t, op1, tx.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(988619), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)

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

	// Raise feerate and trigger new publishing. The tx should be the same.
	setFeeRate(feeRateHigh)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(601))

	tx2 := <-lnd.TxPublishChannel
	require.Equal(t, tx.TxHash(), tx2.TxHash())

	// Now add another input. It is online, but the first input is still
	// offline, so another input should go to another batch.
	swapHash2 := lntypes.Hash{2, 2, 2}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2},
		Index: 2,
	}
	sweepReq2 := SweepRequest{
		SwapHash: swapHash2,
		Inputs: []Input{{
			Value:    2_000_000,
			Outpoint: op2,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op2, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op2, Value: 2_000_000}},
		sweepTimeout, destAddr,
	)
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
	require.Equal(t, int64(1987724), batch2.TxOut[0].Value)
	require.Equal(t, batchPkScript, batch2.TxOut[0].PkScript)

	// Now confirm the first batch. Make sure its presigned transactions
	// were removed, but not the transactions of the second batch.
	presignedSize1 := len(presignedHelper.presignedBatches)

	tx2hash := tx2.TxHash()
	spendDetail := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op1,
		SpendingTx:        tx2,
		SpenderTxHash:     &tx2hash,
		SpenderInputIndex: 0,
		SpendingHeight:    601,
	}
	lnd.SpendChannel <- spendDetail
	<-lnd.RegisterConfChannel
	require.NoError(t, lnd.NotifyHeight(604))
	lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: tx2,
	}

	<-presignedHelper.cleanupCalled

	presignedSize2 := len(presignedHelper.presignedBatches)
	require.Greater(t, presignedSize2, 0)
	require.Greater(t, presignedSize1, presignedSize2)

	// Make sure we still have presigned transactions for the second batch.
	presignedHelper.SetOutpointOnline(op2, false)
	const presignedOnly = true
	_, err = presignedHelper.SignTx(
		ctx, batch2, 2_000_000, chainfee.FeePerKwFloor,
		chainfee.FeePerKwFloor, presignedOnly,
	)
	require.NoError(t, err)
}

// testPresigned_two_inputs_one_goes_offline tests presigned mode for the
// following scenario: two online inputs are added, then one of them goes
// offline, then feerate grows and a presigned transaction is used.
func testPresigned_two_inputs_one_goes_offline(t *testing.T,
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

	presignedHelper := newMockPresignedHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper),
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
		Inputs: []Input{{
			Value:    1_000_000,
			Outpoint: op1,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op1, Value: 1_000_000}},
		sweepTimeout, destAddr,
	)
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
		Inputs: []Input{{
			Value:    2_000_000,
			Outpoint: op2,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op2, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op2, Value: 2_000_000}},
		sweepTimeout, destAddr,
	)
	require.NoError(t, err)
	require.NoError(t, batcher.AddSweep(&sweepReq2))

	// Wait for a transactions to be published.
	tx := <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 2)
	require.Len(t, tx.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2},
		[]wire.OutPoint{
			tx.TxIn[0].PreviousOutPoint,
			tx.TxIn[1].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(2993740), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)

	// Now turn off the second input, raise feerate and trigger new
	// publishing. The feerate is close to one of the presigned feerates,
	// so this should result in RBF.
	presignedHelper.SetOutpointOnline(op2, false)
	setFeeRate(feeRateMedium)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, batcher.AddSweep(&sweepReq2))
	require.NoError(t, lnd.NotifyHeight(601))

	tx2 := <-lnd.TxPublishChannel
	require.NotEqual(t, tx.TxHash(), tx2.TxHash())
	require.Len(t, tx2.TxIn, 2)
	require.Len(t, tx2.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2},
		[]wire.OutPoint{
			tx.TxIn[0].PreviousOutPoint,
			tx.TxIn[1].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(2982008), tx2.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx2.TxOut[0].PkScript)
}

// testPresigned_first_publish_fails tests presigned mode for the following
// scenario: one input is added and goes offline, feerate grows a transaction is
// attempted to be published, but fails. Then the input goes online and is
// published being signed online.
func testPresigned_first_publish_fails(t *testing.T,
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

	presignedHelper := newMockPresignedHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper),
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
		Inputs: []Input{{
			Value:    1_000_000,
			Outpoint: op1,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op1, Value: 1_000_000}},
		sweepTimeout, destAddr,
	)
	require.NoError(t, err)
	presignedHelper.SetOutpointOnline(op1, false)

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

	// Trigger another publish attempt in case the publish error was logged
	// before we installed the logger watcher.
	require.NoError(t, lnd.NotifyHeight(601))

	// Wait for batcher to log the publish error. It is logged with
	// publishErrorHandler, so the format is "%s: %v".
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		testLogger.mu.Lock()
		defer testLogger.mu.Unlock()

		assert.Contains(c, testLogger.warnMessages, "%s: %v")
	}, test.Timeout, eventuallyCheckFrequency)

	// Now turn on the first input, raise feerate and trigger new
	// publishing, which should succeed.
	lnd.PublishHandler = nil
	setFeeRate(feeRateMedium)
	presignedHelper.SetOutpointOnline(op1, true)
	require.NoError(t, batcher.AddSweep(&sweepReq1))
	require.NoError(t, lnd.NotifyHeight(602))

	// Wait for a transactions to be published.
	tx := <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 1)
	require.Len(t, tx.TxOut, 1)
	require.Equal(t, op1, tx.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(988120), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)
}

// testPresigned_locktime tests presigned mode for the following scenario: one
// input is added and goes offline, feerate grows, but this is constrainted by
// locktime logic, so the published transaction has medium feerate (maximum
// feerate among transactions without locktime protection). Then blocks are
// mined and a transaction with a higher feerate is published.
func testPresigned_locktime(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		feeRateLow  = chainfee.SatPerKWeight(10_000)
		feeRateHigh = chainfee.SatPerKWeight(10_000_000)
	)

	currentFeeRate := feeRateLow
	setFeeRate := func(feeRate chainfee.SatPerKWeight) {
		currentFeeRate = feeRate
	}
	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return currentFeeRate, nil
	}

	presignedHelper := newMockPresignedHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper),
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
		Inputs: []Input{{
			Value:    1_000_000,
			Outpoint: op1,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op1, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op1, Value: 1_000_000}},
		sweepTimeout, destAddr,
	)
	require.NoError(t, err)
	presignedHelper.SetOutpointOnline(op1, false)

	setFeeRate(feeRateHigh)

	// Add the sweep, triggering the publish attempt.
	require.NoError(t, batcher.AddSweep(&sweepReq1))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	tx := <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 1)
	require.Len(t, tx.TxOut, 1)
	require.Equal(t, op1, tx.TxIn[0].PreviousOutPoint)
	require.Equal(t, int64(966016), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)

	// Mine blocks to overcome the locktime constraint.
	require.NoError(t, lnd.NotifyHeight(950))

	tx2 := <-lnd.TxPublishChannel
	require.Equal(t, int64(824649), tx2.TxOut[0].Value)
}

// testPresigned_presigned_group tests passing multiple sweeps to the method
// PresignSweepsGroup.
func testPresigned_presigned_group(t *testing.T,
	batcherStore testBatcherStore) {

	defer test.Guard(t)()

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	lnd := test.NewMockLnd()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	customFeeRate := func(_ context.Context,
		_ lntypes.Hash) (chainfee.SatPerKWeight, error) {

		return chainfee.SatPerKWeight(10_000), nil
	}

	presignedHelper := newMockPresignedHelper()

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, &dummySweepFetcherMock{},
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper),
	)

	go func() {
		err := batcher.Run(ctx)
		checkBatcherError(t, err)
	}()

	// Create a swap of two sweeps.
	swapHash1 := lntypes.Hash{1, 1, 1}
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1},
		Index: 1,
	}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2},
		Index: 2,
	}
	group1 := []Input{
		{
			Outpoint: op1,
			Value:    1_000_000,
		},
		{
			Outpoint: op2,
			Value:    2_000_000,
		},
	}

	// Enable only one of the sweeps.
	presignedHelper.SetOutpointOnline(op1, true)
	presignedHelper.SetOutpointOnline(op2, false)

	// An attempt to presign must fail.
	err = batcher.PresignSweepsGroup(ctx, group1, sweepTimeout, destAddr)
	require.ErrorContains(t, err, "some inputs of tx are offline")

	// Enable both outpoints.
	presignedHelper.SetOutpointOnline(op2, true)

	// An attempt to presign must succeed.
	err = batcher.PresignSweepsGroup(ctx, group1, sweepTimeout, destAddr)
	require.NoError(t, err)

	// Add the sweep, triggering the publish attempt.
	require.NoError(t, batcher.AddSweep(&SweepRequest{
		SwapHash: swapHash1,
		Inputs:   group1,
		Notifier: &dummyNotifier,
	}))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	// Wait for a transactions to be published.
	tx := <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 2)
	require.Len(t, tx.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2},
		[]wire.OutPoint{
			tx.TxIn[0].PreviousOutPoint,
			tx.TxIn[1].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(2993740), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)

	// Add another group of sweeps.
	swapHash2 := lntypes.Hash{2, 2, 2}
	op3 := wire.OutPoint{
		Hash:  chainhash.Hash{3, 3},
		Index: 3,
	}
	op4 := wire.OutPoint{
		Hash:  chainhash.Hash{4, 4},
		Index: 4,
	}
	group2 := []Input{
		{
			Outpoint: op3,
			Value:    3_000_000,
		},
		{
			Outpoint: op4,
			Value:    4_000_000,
		},
	}
	presignedHelper.SetOutpointOnline(op3, true)
	presignedHelper.SetOutpointOnline(op4, true)

	// An attempt to presign must succeed.
	err = batcher.PresignSweepsGroup(ctx, group2, sweepTimeout, destAddr)
	require.NoError(t, err)

	// Add the sweep. It should go to the same batch.
	require.NoError(t, batcher.AddSweep(&SweepRequest{
		SwapHash: swapHash2,
		Inputs:   group2,
		Notifier: &dummyNotifier,
	}))

	// Mine a blocks to trigger republishing.
	require.NoError(t, lnd.NotifyHeight(601))

	tx = <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 4)
	require.Len(t, tx.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op1, op2, op3, op4},
		[]wire.OutPoint{
			tx.TxIn[0].PreviousOutPoint,
			tx.TxIn[1].PreviousOutPoint,
			tx.TxIn[2].PreviousOutPoint,
			tx.TxIn[3].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(9989140), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)

	// Turn off one of existing outpoints and add another group.
	presignedHelper.SetOutpointOnline(op1, false)

	swapHash3 := lntypes.Hash{3, 3, 3}
	op5 := wire.OutPoint{
		Hash:  chainhash.Hash{5, 5},
		Index: 5,
	}
	op6 := wire.OutPoint{
		Hash:  chainhash.Hash{6, 6},
		Index: 6,
	}
	group3 := []Input{
		{
			Outpoint: op5,
			Value:    5_000_000,
		},
		{
			Outpoint: op6,
			Value:    6_000_000,
		},
	}
	presignedHelper.SetOutpointOnline(op5, true)
	presignedHelper.SetOutpointOnline(op6, true)

	// An attempt to presign must succeed.
	err = batcher.PresignSweepsGroup(ctx, group3, sweepTimeout, destAddr)
	require.NoError(t, err)

	// Add the sweep. It should go to the same batch.
	require.NoError(t, batcher.AddSweep(&SweepRequest{
		SwapHash: swapHash3,
		Inputs:   group3,
		Notifier: &dummyNotifier,
	}))

	// Since a batch was created we check that it registered for its primary
	// sweep's spend.
	<-lnd.RegisterSpendChannel

	tx = <-lnd.TxPublishChannel
	require.Len(t, tx.TxIn, 2)
	require.Len(t, tx.TxOut, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{op5, op6},
		[]wire.OutPoint{
			tx.TxIn[0].PreviousOutPoint,
			tx.TxIn[1].PreviousOutPoint,
		},
	)
	require.Equal(t, int64(10993740), tx.TxOut[0].Value)
	require.Equal(t, batchPkScript, tx.TxOut[0].PkScript)
}

// testPresigned_presigned_and_regular_sweeps tests a combination of presigned
// mode and regular mode for the following scenario: one regular input is added,
// then a presigned input is added and it goes to another batch, because they
// should not appear in the same batch. Then another regular and another
// presigned inputs are added and go to the existing batches of their types.
func testPresigned_presigned_and_regular_sweeps(t *testing.T, store testStore,
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

	presignedHelper := newMockPresignedHelper()

	sweepStore, err := NewSweepFetcherFromSwapStore(store, lnd.ChainParams)
	require.NoError(t, err)

	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		batcherStore, sweepStore,
		WithCustomFeeRate(customFeeRate),
		WithPresignedHelper(presignedHelper),
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
		Inputs: []Input{{
			Value:    1_000_000,
			Outpoint: op1,
		}},
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

	///////////////////////////////////////
	// Create the first presigned sweep. //
	///////////////////////////////////////
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
		Inputs: []Input{{
			Value:    2_000_000,
			Outpoint: op2,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op2, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op2, Value: 2_000_000}},
		sweepTimeout, destAddr,
	)
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
		Inputs: []Input{{
			Value:    4_000_000,
			Outpoint: op3,
		}},
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

	////////////////////////////////////////
	// Create the second presigned sweep. //
	////////////////////////////////////////
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
		Inputs: []Input{{
			Value:    3_000_000,
			Outpoint: op4,
		}},
		Notifier: &dummyNotifier,
	}
	presignedHelper.SetOutpointOnline(op4, true)
	err = batcher.PresignSweepsGroup(
		ctx, []Input{{Outpoint: op4, Value: 4_000_000}},
		sweepTimeout, destAddr,
	)
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

// TestPresigned tests presigned mode. Most sub-tests doesn't use loopdb.
func TestPresigned(t *testing.T) {
	logger := btclog.NewSLogger(btclog.NewDefaultHandler(os.Stdout))
	logger.SetLevel(btclog.LevelTrace)
	UseLogger(logger.SubSystem("SWEEP"))

	t.Run("input1_offline_then_input2", func(t *testing.T) {
		testPresigned_input1_offline_then_input2(t, NewStoreMock())
	})

	t.Run("two_inputs_one_goes_offline", func(t *testing.T) {
		testPresigned_two_inputs_one_goes_offline(t, NewStoreMock())
	})

	t.Run("first_publish_fails", func(t *testing.T) {
		testPresigned_first_publish_fails(t, NewStoreMock())
	})

	t.Run("locktime", func(t *testing.T) {
		testPresigned_locktime(t, NewStoreMock())
	})

	t.Run("presigned_group", func(t *testing.T) {
		testPresigned_presigned_group(t, NewStoreMock())
	})

	t.Run("presigned_and_regular_sweeps", func(t *testing.T) {
		runTests(t, testPresigned_presigned_and_regular_sweeps)
	})
}
