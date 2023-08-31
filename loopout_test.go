package loop

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestLoopOutPaymentParameters tests the first part of the loop out process up
// to the point where the off-chain payments are made.
func TestLoopOutPaymentParameters(t *testing.T) {
	t.Run("stable protocol", func(t *testing.T) {
		testLoopOutPaymentParameters(t)
	})

	t.Run("experimental protocol", func(t *testing.T) {
		loopdb.EnableExperimentalProtocol()
		defer loopdb.ResetCurrentProtocolVersion()

		testLoopOutPaymentParameters(t)
	})
}

// TestLoopOutPaymentParameters tests the first part of the loop out process up
// to the point where the off-chain payments are made.
func testLoopOutPaymentParameters(t *testing.T) {
	defer test.Guard(t)()

	// Set up test context objects.
	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	server := newServerMock(lnd)
	store := newStoreMock(t)

	expiryChan := make(chan time.Time)
	timerFactory := func(_ time.Duration) <-chan time.Time {
		return expiryChan
	}

	height := int32(600)

	cfg := &swapConfig{
		lnd:    &lnd.LndServices,
		store:  store,
		server: server,
	}

	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}

	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)

	const maxParts = uint32(5)

	chanSet := loopdb.ChannelSet{2, 3}

	// Initiate the swap.
	req := *testRequest
	req.OutgoingChanSet = chanSet

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, height, &req,
	)
	require.NoError(t, err)
	swap := initResult.swap

	// Execute the swap in its own goroutine.
	errChan := make(chan error)
	swapCtx, cancel := context.WithCancel(context.Background())

	go func() {
		err := swap.execute(swapCtx, nil, &executeConfig{
			statusChan:       statusChan,
			sweeper:          sweeper,
			blockEpochChan:   blockEpochChan,
			timerFactory:     timerFactory,
			loopOutMaxParts:  maxParts,
			cancelSwap:       server.CancelLoopOutSwap,
			verifySchnorrSig: mockVerifySchnorrSigFail,
		}, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	store.assertLoopOutStored()

	state := <-statusChan
	require.Equal(t, loopdb.StateInitiated, state.State)

	// Check that the SwapInfo contains the outgoing chan set
	require.Equal(t, chanSet, state.OutgoingChanSet)

	// Check that the SwapInfo does not contain a last hop
	require.Nil(t, state.LastHop)

	// Intercept the swap and prepay payments. Order is undefined.
	payments := []test.RouterPaymentChannelMessage{
		<-ctx.Lnd.RouterSendPaymentChannel,
		<-ctx.Lnd.RouterSendPaymentChannel,
	}

	// Find the swap payment.
	var swapPayment test.RouterPaymentChannelMessage
	for _, p := range payments {
		if p.Invoice == swap.SwapInvoice {
			swapPayment = p
		}
	}

	// Assert that it is sent as a multi-part payment.
	require.Equal(t, maxParts, swapPayment.MaxParts)

	// Verify the outgoing channel set restriction.
	require.Equal(
		t, []uint64(req.OutgoingChanSet), swapPayment.OutgoingChanIds,
	)

	// Swap is expected to register for confirmation of the htlc. Assert
	// this to prevent a blocked channel in the mock.
	ctx.AssertRegisterConf(false, defaultConfirmations)

	// Cancel the swap. There is nothing else we need to assert. The payment
	// parameters don't play a role in the remainder of the swap process.
	cancel()

	// Expect the swap to signal that it was cancelled.
	require.Equal(t, context.Canceled, <-errChan)
}

// TestLateHtlcPublish tests that the client is not revealing the preimage if
// there are not enough blocks left.
func TestLateHtlcPublish(t *testing.T) {
	t.Run("stable protocol", func(t *testing.T) {
		testLateHtlcPublish(t)
	})

	t.Run("experimental protocol", func(t *testing.T) {
		loopdb.EnableExperimentalProtocol()
		defer loopdb.ResetCurrentProtocolVersion()

		testLateHtlcPublish(t)
	})
}

func testLateHtlcPublish(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()

	ctx := test.NewContext(t, lnd)

	server := newServerMock(lnd)

	store := newStoreMock(t)

	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	height := int32(600)

	cfg := newSwapConfig(&lnd.LndServices, store, server)

	testRequest.Expiry = height + testLoopOutMinOnChainCltvDelta

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, height, testRequest,
	)
	require.NoError(t, err)
	swap := initResult.swap

	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}

	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), nil, &executeConfig{
			statusChan:       statusChan,
			sweeper:          sweeper,
			blockEpochChan:   blockEpochChan,
			timerFactory:     timerFactory,
			cancelSwap:       server.CancelLoopOutSwap,
			verifySchnorrSig: mockVerifySchnorrSigFail,
		}, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	store.assertLoopOutStored()
	status := <-statusChan
	require.Equal(t, loopdb.StateInitiated, status.State)

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf
	ctx.AssertRegisterConf(false, defaultConfirmations)

	// // Wait too long before publishing htlc.
	blockEpochChan <- swap.CltvExpiry - 10

	signalSwapPaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	signalPrepaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)

	store.assertStoreFinished(loopdb.StateFailTimeout)

	status = <-statusChan
	require.Equal(t, loopdb.StateFailTimeout, status.State)
	require.NoError(t, <-errChan)
}

// TestCustomSweepConfTarget ensures we are able to sweep a Loop Out HTLC with a
// custom confirmation target.
func TestCustomSweepConfTarget(t *testing.T) {
	t.Run("stable protocol", func(t *testing.T) {
		testCustomSweepConfTarget(t)
	})

	t.Run("experimental protocol", func(t *testing.T) {
		loopdb.EnableExperimentalProtocol()
		defer loopdb.ResetCurrentProtocolVersion()

		testCustomSweepConfTarget(t)
	})
}

func testCustomSweepConfTarget(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	server := newServerMock(lnd)

	// Use the highest sweep confirmation target before we attempt to use
	// the default.
	testReq := *testRequest

	testReq.SweepConfTarget = testLoopOutMinOnChainCltvDelta -
		DefaultSweepConfTargetDelta - 1

	// Set on-chain HTLC CLTV.
	testReq.Expiry = ctx.Lnd.Height + testLoopOutMinOnChainCltvDelta

	// Set up custom fee estimates such that the lower confirmation target
	// yields a much higher fee rate.
	ctx.Lnd.SetFeeEstimate(testReq.SweepConfTarget, 250)
	ctx.Lnd.SetFeeEstimate(DefaultSweepConfTarget, 10000)

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), server,
	)

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, ctx.Lnd.Height, &testReq,
	)
	require.NoError(t, err)
	swap := initResult.swap

	// Set up the required dependencies to execute the swap.
	//
	// TODO: create test context similar to loopInTestContext.
	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}
	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)
	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), nil, &executeConfig{
			statusChan:       statusChan,
			blockEpochChan:   blockEpochChan,
			timerFactory:     timerFactory,
			sweeper:          sweeper,
			cancelSwap:       server.CancelLoopOutSwap,
			verifySchnorrSig: mockVerifySchnorrSigFail,
		}, ctx.Lnd.Height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	// The swap should be found in its initial state.
	cfg.store.(*storeMock).assertLoopOutStored()
	state := <-statusChan
	require.Equal(t, loopdb.StateInitiated, state.State)

	// We'll then pay both the swap and prepay invoice, which should trigger
	// the server to publish the on-chain HTLC.
	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	signalSwapPaymentResult(nil)
	signalPrepaymentResult(nil)

	// Notify the confirmation notification for the HTLC.
	ctx.AssertRegisterConf(false, defaultConfirmations)

	blockEpochChan <- ctx.Lnd.Height + 1

	htlcTx := wire.NewMsgTx(2)
	htlcTx.AddTxOut(&wire.TxOut{
		Value:    int64(swap.AmountRequested),
		PkScript: swap.htlc.PkScript,
	})

	ctx.NotifyConf(htlcTx)

	// The client should then register for a spend of the HTLC and attempt
	// to sweep it using the custom confirmation target.
	ctx.AssertRegisterSpendNtfn(swap.htlc.PkScript)

	// Assert that we made a query to track our payment, as required for
	// preimage push tracking.
	trackPayment := ctx.AssertTrackPayment()

	expiryChan <- time.Now()

	// Expect a signing request for the HTLC success transaction.
	if !IsTaprootSwap(&swap.SwapContract) {
		<-ctx.Lnd.SignOutputRawChannel
	}

	cfg.store.(*storeMock).assertLoopOutState(loopdb.StatePreimageRevealed)
	status := <-statusChan
	require.Equal(t, loopdb.StatePreimageRevealed, status.State)

	// When using taproot htlcs the flow is different as we do reveal the
	// preimage before sweeping in order for the server to trust us with
	// our MuSig2 signing attempts.
	if IsTaprootSwap(&swap.SwapContract) {
		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)

		// Try MuSig2 signing first and fail it so that we go for a
		// normal sweep.
		for i := 0; i < maxMusigSweepRetries; i++ {
			expiryChan <- time.Now()
			preimage := <-server.preimagePush
			require.Equal(t, swap.Preimage, preimage)
		}

		<-ctx.Lnd.SignOutputRawChannel
	}

	// assertSweepTx performs some sanity checks on a sweep transaction to
	// ensure it was constructed correctly.
	assertSweepTx := func(expConfTarget int32) *wire.MsgTx {
		t.Helper()

		sweepTx := ctx.ReceiveTx()
		require.Equal(
			t, htlcTx.TxHash(),
			sweepTx.TxIn[0].PreviousOutPoint.Hash,
		)

		// The fee used for the sweep transaction is an estimate based
		// on the maximum witness size, so we should expect to see a
		// lower fee when using the actual witness size of the
		// transaction.
		fee := btcutil.Amount(
			htlcTx.TxOut[0].Value - sweepTx.TxOut[0].Value,
		)

		weight := blockchain.GetTransactionWeight(btcutil.NewTx(sweepTx))
		feeRate, err := ctx.Lnd.WalletKit.EstimateFeeRate(
			context.Background(), expConfTarget,
		)
		require.NoError(t, err, "unable to retrieve fee estimate")

		minFee := feeRate.FeeForWeight(weight)
		// Just an estimate that works to sanity check fee upper bound.
		maxFee := btcutil.Amount(float64(minFee) * 1.5)

		require.GreaterOrEqual(t, fee, minFee)
		require.LessOrEqual(t, fee, maxFee)

		return sweepTx
	}

	// The sweep should have a fee that corresponds to the custom
	// confirmation target.
	_ = assertSweepTx(testReq.SweepConfTarget)

	// Once we have published an on chain sweep, we expect a preimage to
	// have been pushed to our server.
	if !IsTaprootSwap(&swap.SwapContract) {
		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)
	}

	// Now that we have pushed our preimage to the sever, we send an update
	// indicating that our off chain htlc is settled. We do this so that
	// we don't have to keep consuming preimage pushes from our server mock
	// for every sweep attempt.
	trackPayment.Updates <- lndclient.PaymentStatus{
		State: lnrpc.Payment_SUCCEEDED,
	}

	// We'll then notify the height at which we begin using the default
	// confirmation target.
	defaultConfTargetHeight := ctx.Lnd.Height +
		testLoopOutMinOnChainCltvDelta - DefaultSweepConfTargetDelta
	blockEpochChan <- defaultConfTargetHeight
	expiryChan <- time.Now()

	// Expect another signing request.
	<-ctx.Lnd.SignOutputRawChannel

	// We should expect to see another sweep using the higher fee since the
	// spend hasn't been confirmed yet.
	sweepTx := assertSweepTx(DefaultSweepConfTarget)

	// Notify the spend so that the swap reaches its final state.
	ctx.NotifySpend(sweepTx, 0)

	cfg.store.(*storeMock).assertLoopOutState(loopdb.StateSuccess)
	status = <-statusChan
	require.Equal(t, loopdb.StateSuccess, status.State)
	require.NoError(t, <-errChan)
}

// TestPreimagePush tests or logic that decides whether to push our preimage to
// the server. First, we test the case where we have not yet disclosed our
// preimage with a sweep, so we do not want to push our preimage yet. Next, we
// broadcast a sweep attempt and push our preimage to the server. In this stage
// we mock a server failure by not sending a settle update for our payment.
// Finally, we make a last sweep attempt, push the preimage (because we have
// not detected our settle) and settle the off chain htlc, indicating that the
// server successfully settled using the preimage push. In this test, we need
// to start with a fee rate that will be too high, then progress to an
// acceptable one.
func TestPreimagePush(t *testing.T) {
	t.Run("stable protocol", func(t *testing.T) {
		testPreimagePush(t)
	})

	t.Run("experimental protocol", func(t *testing.T) {
		loopdb.EnableExperimentalProtocol()
		defer loopdb.ResetCurrentProtocolVersion()

		testPreimagePush(t)
	})
}

func testPreimagePush(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	server := newServerMock(lnd)

	testReq := *testRequest
	testReq.SweepConfTarget = 10
	testReq.Expiry = ctx.Lnd.Height + testLoopOutMinOnChainCltvDelta

	// We set our mock fee estimate for our target sweep confs to be our
	// max miner fee *2, so that our fee will definitely be above what we
	// are willing to pay, and we will not sweep.
	ctx.Lnd.SetFeeEstimate(
		testReq.SweepConfTarget, chainfee.SatPerKWeight(
			testReq.MaxMinerFee*2,
		),
	)

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), server,
	)

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, ctx.Lnd.Height, &testReq,
	)
	require.NoError(t, err)
	swap := initResult.swap

	// Set up the required dependencies to execute the swap.
	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}
	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)
	expiryChan := make(chan time.Time)
	timerFactory := func(_ time.Duration) <-chan time.Time {
		return expiryChan
	}

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), nil, &executeConfig{
			statusChan:       statusChan,
			blockEpochChan:   blockEpochChan,
			timerFactory:     timerFactory,
			sweeper:          sweeper,
			cancelSwap:       server.CancelLoopOutSwap,
			verifySchnorrSig: mockVerifySchnorrSigFail,
		}, ctx.Lnd.Height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	// The swap should be found in its initial state.
	cfg.store.(*storeMock).assertLoopOutStored()
	state := <-statusChan
	require.Equal(t, loopdb.StateInitiated, state.State)

	// We'll then pay both the swap and prepay invoice, which should trigger
	// the server to publish the on-chain HTLC.
	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	signalSwapPaymentResult(nil)
	signalPrepaymentResult(nil)

	// Notify the confirmation notification for the HTLC.
	ctx.AssertRegisterConf(false, defaultConfirmations)

	blockEpochChan <- ctx.Lnd.Height + 1

	htlcTx := wire.NewMsgTx(2)
	htlcTx.AddTxOut(&wire.TxOut{
		Value:    int64(swap.AmountRequested),
		PkScript: swap.htlc.PkScript,
	})

	ctx.NotifyConf(htlcTx)

	// The client should then register for a spend of the HTLC and attempt
	// to sweep it using the custom confirmation target.
	ctx.AssertRegisterSpendNtfn(swap.htlc.PkScript)

	// Assert that we made a query to track our payment, as required for
	// preimage push tracking.
	trackPayment := ctx.AssertTrackPayment()

	// Tick the expiry channel, we are still using our client confirmation
	// target at this stage which has fees higher than our max acceptable
	// fee. We do not expect a sweep attempt at this point. Since our
	// preimage is not revealed, we also do not expect a preimage push.
	expiryChan <- testTime

	// When using taproot htlcs the flow is different as we do reveal the
	// preimage before sweeping in order for the server to trust us with
	// our MuSig2 signing attempts.
	if IsTaprootSwap(&swap.SwapContract) {
		cfg.store.(*storeMock).assertLoopOutState(
			loopdb.StatePreimageRevealed,
		)
		status := <-statusChan
		require.Equal(
			t, status.State, loopdb.StatePreimageRevealed,
		)

		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)

		// Try MuSig2 signing first and fail it so that we go for a
		// normal sweep.
		for i := 0; i < maxMusigSweepRetries; i++ {
			expiryChan <- time.Now()

			preimage := <-server.preimagePush
			require.Equal(t, swap.Preimage, preimage)
		}

		<-ctx.Lnd.SignOutputRawChannel

		// We expect the sweep tx to have been published.
		ctx.ReceiveTx()
	}

	// Since we don't have a reliable mechanism to non-intrusively avoid
	// races by setting the fee estimate too soon, let's sleep here a bit
	// to ensure the first sweep fails.
	time.Sleep(500 * time.Millisecond)

	// Now we decrease our fees for the swap's confirmation target to less
	// than the maximum miner fee.
	ctx.Lnd.SetFeeEstimate(testReq.SweepConfTarget, chainfee.SatPerKWeight(
		testReq.MaxMinerFee/2,
	))

	// Now when we report a new block and tick our expiry fee timer, and
	// fees are acceptably low so we expect our sweep to be published.
	blockEpochChan <- ctx.Lnd.Height + 2
	expiryChan <- testTime

	if IsTaprootSwap(&swap.SwapContract) {
		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)
	}

	// Expect a signing request for the HTLC success transaction.
	<-ctx.Lnd.SignOutputRawChannel

	if !IsTaprootSwap(&swap.SwapContract) {
		// This is the first time we have swept, so we expect our
		// preimage revealed state to be set.
		cfg.store.(*storeMock).assertLoopOutState(
			loopdb.StatePreimageRevealed,
		)
		status := <-statusChan
		require.Equal(
			t, status.State, loopdb.StatePreimageRevealed,
		)
	}

	// We expect the sweep tx to have been published.
	ctx.ReceiveTx()

	if !IsTaprootSwap(&swap.SwapContract) {
		// Once we have published an on chain sweep, we expect a
		// preimage to have been pushed to the server after the sweep.
		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)
	}

	// To mock a server failure, we do not send a payment settled update
	// for our off chain payment yet. We also do not confirm our sweep on
	// chain yet so we can test our preimage push retry logic. Instead, we
	// tick the expiry chan again to prompt another sweep.
	expiryChan <- testTime
	if IsTaprootSwap(&swap.SwapContract) {
		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)
	}

	// We expect another signing request for out sweep, and publish of our
	// sweep transaction.
	<-ctx.Lnd.SignOutputRawChannel
	ctx.ReceiveTx()

	// Since we have not yet been notified of an off chain settle, and we
	// have attempted to sweep again, we expect another preimage push
	// attempt.

	if !IsTaprootSwap(&swap.SwapContract) {
		preimage := <-server.preimagePush
		require.Equal(t, swap.Preimage, preimage)
	}

	// This time, we send a payment succeeded update into our payment stream
	// to reflect that the server received our preimage push and settled off
	// chain.
	trackPayment.Updates <- lndclient.PaymentStatus{
		State: lnrpc.Payment_SUCCEEDED,
	}

	// We tick one last time, this time expecting a sweep but no preimage
	// push. The test's mocked preimage channel is un-buffered, so our test
	// would hang if we pushed the preimage here.
	expiryChan <- testTime
	<-ctx.Lnd.SignOutputRawChannel
	sweepTx := ctx.ReceiveTx()

	// Finally, we put this swap out of its misery and notify a successful
	// spend our our sweepTx and assert that the swap succeeds.
	ctx.NotifySpend(sweepTx, 0)

	cfg.store.(*storeMock).assertLoopOutState(loopdb.StateSuccess)
	status := <-statusChan
	require.Equal(
		t, status.State, loopdb.StateSuccess,
	)

	require.NoError(t, <-errChan)
}

// TestFailedOffChainCancelation tests sending of a cancelation message to
// the server when a swap fails due to off-chain routing.
func TestFailedOffChainCancelation(t *testing.T) {
	t.Run("stable protocol", func(t *testing.T) {
		testFailedOffChainCancelation(t)
	})

	t.Run("experimental protocol", func(t *testing.T) {
		loopdb.EnableExperimentalProtocol()
		defer loopdb.ResetCurrentProtocolVersion()

		testFailedOffChainCancelation(t)
	})
}

func testFailedOffChainCancelation(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	server := newServerMock(lnd)

	testReq := *testRequest
	testReq.Expiry = lnd.Height + 20

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), server,
	)

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, lnd.Height, &testReq,
	)
	require.NoError(t, err)
	swap := initResult.swap

	// Set up the required dependencies to execute the swap.
	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}
	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)
	expiryChan := make(chan time.Time)
	timerFactory := func(_ time.Duration) <-chan time.Time {
		return expiryChan
	}

	errChan := make(chan error)
	go func() {
		cfg := &executeConfig{
			statusChan:       statusChan,
			sweeper:          sweeper,
			blockEpochChan:   blockEpochChan,
			timerFactory:     timerFactory,
			cancelSwap:       server.CancelLoopOutSwap,
			verifySchnorrSig: mockVerifySchnorrSigFail,
		}

		err := swap.execute(
			context.Background(), nil, cfg, ctx.Lnd.Height,
		)
		errChan <- err
	}()

	// The swap should be found in its initial state.
	cfg.store.(*storeMock).assertLoopOutStored()
	state := <-statusChan
	require.Equal(t, loopdb.StateInitiated, state.State)

	// Assert that we register for htlc confirmation notifications.
	ctx.AssertRegisterConf(false, defaultConfirmations)

	// We expect prepayment and invoice to be dispatched, order is unknown.
	pmt1 := <-ctx.Lnd.RouterSendPaymentChannel
	pmt2 := <-ctx.Lnd.RouterSendPaymentChannel

	failUpdate := lndclient.PaymentStatus{
		State:         lnrpc.Payment_FAILED,
		FailureReason: lnrpc.PaymentFailureReason_FAILURE_REASON_ERROR,
		Htlcs: []*lndclient.HtlcAttempt{
			{
				// Include a non-failed htlc to test that we
				// only report failed htlcs.
				Status: lnrpc.HTLCAttempt_IN_FLIGHT,
			},
			// Add one htlc that failed within the server's
			// infrastructure.
			{
				Status: lnrpc.HTLCAttempt_FAILED,
				Route: &lnrpc.Route{
					Hops: []*lnrpc.Hop{
						{}, {}, {},
					},
				},
				Failure: &lndclient.HtlcFailure{
					FailureSourceIndex: 1,
				},
			},
			// Add one htlc that failed in the network at wide.
			{
				Status: lnrpc.HTLCAttempt_FAILED,
				Route: &lnrpc.Route{
					Hops: []*lnrpc.Hop{
						{}, {}, {}, {}, {},
					},
				},
				Failure: &lndclient.HtlcFailure{
					FailureSourceIndex: 1,
				},
			},
		},
	}

	successUpdate := lndclient.PaymentStatus{
		State: lnrpc.Payment_SUCCEEDED,
	}

	// We want to fail our swap payment and succeed the prepush, so we send
	// a failure update to the payment that has the larger amount.
	if pmt1.Amount > pmt2.Amount {
		pmt1.TrackPaymentMessage.Updates <- failUpdate
		pmt2.TrackPaymentMessage.Updates <- successUpdate
	} else {
		pmt1.TrackPaymentMessage.Updates <- successUpdate
		pmt2.TrackPaymentMessage.Updates <- failUpdate
	}

	invoice, err := zpay32.Decode(
		swap.LoopOutContract.SwapInvoice, lnd.ChainParams,
	)
	require.NoError(t, err)
	require.NotNil(t, invoice.PaymentAddr)

	swapCancelation := &outCancelDetails{
		hash:        swap.hash,
		paymentAddr: *invoice.PaymentAddr,
		metadata: routeCancelMetadata{
			paymentType:   paymentTypeInvoice,
			failureReason: failUpdate.FailureReason,
			attempts: []uint32{
				2,
				math.MaxUint32,
			},
		},
	}
	server.assertSwapCanceled(t, swapCancelation)

	// Finally, the swap should be recorded with failed off chain timeout.
	cfg.store.(*storeMock).assertLoopOutState(
		loopdb.StateFailOffchainPayments,
	)
	state = <-statusChan
	require.Equal(t, state.State, loopdb.StateFailOffchainPayments)
	require.NoError(t, <-errChan)
}

// TestLoopOutMuSig2Sweep tests the loop out sweep flow when the MuSig2 signing
// process is successful.
func TestLoopOutMuSig2Sweep(t *testing.T) {
	defer test.Guard(t)()

	// TODO(bhandras): remove when MuSig2 is default.
	loopdb.EnableExperimentalProtocol()
	defer loopdb.ResetCurrentProtocolVersion()

	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	server := newServerMock(lnd)

	testReq := *testRequest
	testReq.SweepConfTarget = 10
	testReq.Expiry = ctx.Lnd.Height + testLoopOutMinOnChainCltvDelta

	// We set our mock fee estimate for our target sweep confs to be our
	// max miner fee * 2. With MuSig2 we still expect that the client will
	// publish the sweep but with the fee clamped to the maximum allowed
	// miner fee as the preimage is revealed before the sweep txn is
	// published.
	ctx.Lnd.SetFeeEstimate(
		testReq.SweepConfTarget, chainfee.SatPerKWeight(
			testReq.MaxMinerFee*2,
		),
	)

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), server,
	)

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, ctx.Lnd.Height, &testReq,
	)
	require.NoError(t, err)
	swap := initResult.swap

	// Set up the required dependencies to execute the swap.
	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}
	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)
	expiryChan := make(chan time.Time)
	timerFactory := func(_ time.Duration) <-chan time.Time {
		return expiryChan
	}

	errChan := make(chan error)

	// Mock a successful signature verify to make sure we don't fail
	// creating the MuSig2 sweep.
	mockVerifySchnorrSigSuccess := func(pubKey *btcec.PublicKey, hash,
		sig []byte) error {

		return nil
	}

	go func() {
		err := swap.execute(context.Background(), nil, &executeConfig{
			statusChan:       statusChan,
			blockEpochChan:   blockEpochChan,
			timerFactory:     timerFactory,
			sweeper:          sweeper,
			cancelSwap:       server.CancelLoopOutSwap,
			verifySchnorrSig: mockVerifySchnorrSigSuccess,
		}, ctx.Lnd.Height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	// The swap should be found in its initial state.
	cfg.store.(*storeMock).assertLoopOutStored()
	state := <-statusChan
	require.Equal(t, loopdb.StateInitiated, state.State)

	// We'll then pay both the swap and prepay invoice, which should trigger
	// the server to publish the on-chain HTLC.
	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	signalSwapPaymentResult(nil)
	signalPrepaymentResult(nil)

	// Notify the confirmation notification for the HTLC.
	ctx.AssertRegisterConf(false, defaultConfirmations)

	blockEpochChan <- ctx.Lnd.Height + 1

	htlcTx := wire.NewMsgTx(2)
	htlcTx.AddTxOut(&wire.TxOut{
		Value:    int64(swap.AmountRequested),
		PkScript: swap.htlc.PkScript,
	})

	ctx.NotifyConf(htlcTx)

	// The client should then register for a spend of the HTLC and attempt
	// to sweep it using the custom confirmation target.
	ctx.AssertRegisterSpendNtfn(swap.htlc.PkScript)

	// Assert that we made a query to track our payment, as required for
	// preimage push tracking.
	trackPayment := ctx.AssertTrackPayment()

	// Tick the expiry channel, we are still using our client confirmation
	// target at this stage which has fees higher than our max acceptable
	// fee. We do not expect a sweep attempt at this point. Since our
	// preimage is not revealed, we also do not expect a preimage push.
	expiryChan <- testTime

	// When using taproot htlcs the flow is different as we do reveal the
	// preimage before sweeping in order for the server to trust us with
	// our MuSig2 signing attempts.
	cfg.store.(*storeMock).assertLoopOutState(
		loopdb.StatePreimageRevealed,
	)
	status := <-statusChan
	require.Equal(
		t, status.State, loopdb.StatePreimageRevealed,
	)

	preimage := <-server.preimagePush
	require.Equal(t, swap.Preimage, preimage)

	// We expect the sweep tx to have been published.
	ctx.ReceiveTx()

	// Since we don't have a reliable mechanism to non-intrusively avoid
	// races by setting the fee estimate too soon, let's sleep here a bit
	// to ensure the first sweep fails.
	time.Sleep(500 * time.Millisecond)

	// Now we decrease our fees for the swap's confirmation target to less
	// than the maximum miner fee.
	ctx.Lnd.SetFeeEstimate(testReq.SweepConfTarget, chainfee.SatPerKWeight(
		testReq.MaxMinerFee/2,
	))

	// Now when we report a new block and tick our expiry fee timer, and
	// fees are acceptably low so we expect our sweep to be published.
	blockEpochChan <- ctx.Lnd.Height + 2
	expiryChan <- testTime

	preimage = <-server.preimagePush
	require.Equal(t, swap.Preimage, preimage)

	// We expect the sweep tx to have been published.
	sweepTx := ctx.ReceiveTx()

	// This time, we send a payment succeeded update into our payment stream
	// to reflect that the server received our preimage push and settled off
	// chain.
	trackPayment.Updates <- lndclient.PaymentStatus{
		State: lnrpc.Payment_SUCCEEDED,
	}

	// Make sure our sweep tx has a single witness indicating keyspend.
	require.Len(t, sweepTx.TxIn[0].Witness, 1)

	// Finally, we put this swap out of its misery and notify a successful
	// spend our our sweepTx and assert that the swap succeeds.
	ctx.NotifySpend(sweepTx, 0)

	cfg.store.(*storeMock).assertLoopOutState(loopdb.StateSuccess)
	status = <-statusChan
	require.Equal(t, status.State, loopdb.StateSuccess)
	require.NoError(t, <-errChan)
}
