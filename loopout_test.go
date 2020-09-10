package loop

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/server"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestLoopOutPaymentParameters tests the first part of the loop out process up
// to the point where the off-chain payments are made.
func TestLoopOutPaymentParameters(t *testing.T) {
	defer test.Guard(t)()

	// Set up test context objects.
	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	mockServer := server.NewServerMock()
	store := newStoreMock(t)

	expiryChan := make(chan time.Time)
	timerFactory := func(_ time.Duration) <-chan time.Time {
		return expiryChan
	}

	height := int32(600)

	cfg := &swapConfig{
		lnd:    &lnd.LndServices,
		store:  store,
		server: mockServer,
	}

	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}

	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)

	const maxParts = 5

	// Initiate the swap.
	req := *testRequest
	req.OutgoingChanSet = loopdb.ChannelSet{2, 3}

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, height, &req,
	)
	if err != nil {
		t.Fatal(err)
	}
	swap := initResult.swap

	// Execute the swap in its own goroutine.
	errChan := make(chan error)
	swapCtx, cancel := context.WithCancel(context.Background())

	go func() {
		err := swap.execute(swapCtx, &executeConfig{
			statusChan:      statusChan,
			sweeper:         sweeper,
			blockEpochChan:  blockEpochChan,
			timerFactory:    timerFactory,
			loopOutMaxParts: maxParts,
		}, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	store.assertLoopOutStored()

	state := <-statusChan
	if state.State != loopdb.StateInitiated {
		t.Fatal("unexpected state")
	}

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
	if swapPayment.MaxParts != maxParts {
		t.Fatalf("Expected %v parts, but got %v",
			maxParts, swapPayment.MaxParts)
	}

	// Verify the outgoing channel set restriction.
	if !reflect.DeepEqual(
		[]uint64(req.OutgoingChanSet), swapPayment.OutgoingChanIds,
	) {
		t.Fatalf("Unexpected outgoing channel set")
	}

	// Swap is expected to register for confirmation of the htlc. Assert
	// this to prevent a blocked channel in the mock.
	ctx.AssertRegisterConf(false, defaultConfirmations)

	// Cancel the swap. There is nothing else we need to assert. The payment
	// parameters don't play a role in the remainder of the swap process.
	cancel()

	// Expect the swap to signal that it was cancelled.
	err = <-errChan
	if err != context.Canceled {
		t.Fatal(err)
	}
}

// TestLateHtlcPublish tests that the client is not revealing the preimage if
// there are not enough blocks left.
func TestLateHtlcPublish(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()

	ctx := test.NewContext(t, lnd)

	mockServer := server.NewServerMock()

	store := newStoreMock(t)

	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	height := int32(600)

	cfg := newSwapConfig(&lnd.LndServices, store, mockServer)

	testRequest.Expiry = height + server.TestLoopOutMinOnChainCltvDelta

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, height, testRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	swap := initResult.swap

	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}

	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), &executeConfig{
			statusChan:     statusChan,
			sweeper:        sweeper,
			blockEpochChan: blockEpochChan,
			timerFactory:   timerFactory,
		}, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	store.assertLoopOutStored()

	state := <-statusChan
	if state.State != loopdb.StateInitiated {
		t.Fatal("unexpected state")
	}

	signalSwapPaymentResult := ctx.AssertPaid(server.SwapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(server.PrepayInvoiceDesc)

	// Expect client to register for conf
	ctx.AssertRegisterConf(false, defaultConfirmations)

	// // Wait too long before publishing htlc.
	blockEpochChan <- int32(swap.CltvExpiry - 10)

	signalSwapPaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	signalPrepaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)

	store.assertStoreFinished(loopdb.StateFailTimeout)

	status := <-statusChan
	if status.State != loopdb.StateFailTimeout {
		t.Fatal("unexpected state")
	}

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}

// TestCustomSweepConfTarget ensures we are able to sweep a Loop Out HTLC with a
// custom confirmation target.
func TestCustomSweepConfTarget(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	mockServer := server.NewServerMock()

	// Use the highest sweep confirmation target before we attempt to use
	// the default.
	testReq := *testRequest

	testReq.SweepConfTarget = server.TestLoopOutMinOnChainCltvDelta -
		DefaultSweepConfTargetDelta - 1

	// Set up custom fee estimates such that the lower confirmation target
	// yields a much higher fee rate.
	ctx.Lnd.SetFeeEstimate(testReq.SweepConfTarget, 250)
	ctx.Lnd.SetFeeEstimate(DefaultSweepConfTarget, 10000)

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), mockServer,
	)

	initResult, err := newLoopOutSwap(
		context.Background(), cfg, ctx.Lnd.Height, &testReq,
	)
	if err != nil {
		t.Fatal(err)
	}
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
		err := swap.execute(context.Background(), &executeConfig{
			statusChan:     statusChan,
			blockEpochChan: blockEpochChan,
			timerFactory:   timerFactory,
			sweeper:        sweeper,
		}, ctx.Lnd.Height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	// The swap should be found in its initial state.
	cfg.store.(*storeMock).assertLoopOutStored()
	state := <-statusChan
	if state.State != loopdb.StateInitiated {
		t.Fatal("unexpected state")
	}

	// We'll then pay both the swap and prepay invoice, which should trigger
	// the server to publish the on-chain HTLC.
	signalSwapPaymentResult := ctx.AssertPaid(server.SwapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(server.PrepayInvoiceDesc)

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
	<-ctx.Lnd.SignOutputRawChannel

	cfg.store.(*storeMock).assertLoopOutState(loopdb.StatePreimageRevealed)
	status := <-statusChan
	if status.State != loopdb.StatePreimageRevealed {
		t.Fatalf("expected state %v, got %v",
			loopdb.StatePreimageRevealed, status.State)
	}

	// assertSweepTx performs some sanity checks on a sweep transaction to
	// ensure it was constructed correctly.
	assertSweepTx := func(expConfTarget int32) *wire.MsgTx {
		t.Helper()

		sweepTx := ctx.ReceiveTx()
		if sweepTx.TxIn[0].PreviousOutPoint.Hash != htlcTx.TxHash() {
			t.Fatalf("expected sweep tx to spend %v, got %v",
				htlcTx.TxHash(), sweepTx.TxIn[0].PreviousOutPoint)
		}

		// The fee used for the sweep transaction is an estimate based
		// on the maximum witness size, so we should expect to see a
		// lower fee when using the actual witness size of the
		// transaction.
		fee := btcutil.Amount(
			htlcTx.TxOut[0].Value - sweepTx.TxOut[0].Value,
		)

		weight := blockchain.GetTransactionWeight(btcutil.NewTx(sweepTx))
		feeRate, err := ctx.Lnd.WalletKit.EstimateFee(
			context.Background(), expConfTarget,
		)
		if err != nil {
			t.Fatalf("unable to retrieve fee estimate: %v", err)
		}
		minFee := feeRate.FeeForWeight(weight)
		maxFee := btcutil.Amount(float64(minFee) * 1.1)

		if fee < minFee && fee > maxFee {
			t.Fatalf("expected sweep tx to have fee between %v-%v, "+
				"got %v", minFee, maxFee, fee)
		}

		return sweepTx
	}

	// The sweep should have a fee that corresponds to the custom
	// confirmation target.
	_ = assertSweepTx(testReq.SweepConfTarget)

	// Once we have published an on chain sweep, we expect a preimage to
	// have been pushed to our server.
	preimage := <-mockServer.PreimagePush
	require.Equal(t, swap.Preimage, preimage)

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
		server.TestLoopOutMinOnChainCltvDelta - DefaultSweepConfTargetDelta
	blockEpochChan <- int32(defaultConfTargetHeight)
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
	if status.State != loopdb.StateSuccess {
		t.Fatalf("expected state %v, got %v", loopdb.StateSuccess,
			status.State)
	}

	if err := <-errChan; err != nil {
		t.Fatal(err)
	}
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
// acceptable one. We do this by starting with a high confirmation target with
// a high fee, and setting the default confirmation fee (which our swap will
// drop down to if it is not confirming in time) to a lower fee. This is not
// intuitive (lower confs having lower fees), but it allows up to mock fee
// changes.
func TestPreimagePush(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	mockServer := server.NewServerMock()

	// Start with a high confirmation delta which will have a very high fee
	// attached to it.
	testReq := *testRequest
	testReq.SweepConfTarget = server.TestLoopOutMinOnChainCltvDelta -
		DefaultSweepConfTargetDelta - 1
	testReq.Expiry = ctx.Lnd.Height + server.TestLoopOutMinOnChainCltvDelta

	// We set our mock fee estimate for our target sweep confs to be our
	// max miner fee *2, so that our fee will definitely be above what we
	// are willing to pay, and we will not sweep.
	ctx.Lnd.SetFeeEstimate(
		testReq.SweepConfTarget, chainfee.SatPerKWeight(
			testReq.MaxMinerFee*2,
		),
	)

	// We set the fee estimate for our default confirmation target very
	// low, so that once we drop down to our default confs we will start
	// trying to sweep the preimage.
	ctx.Lnd.SetFeeEstimate(DefaultSweepConfTarget, 1)

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), mockServer,
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
		err := swap.execute(context.Background(), &executeConfig{
			statusChan:     statusChan,
			blockEpochChan: blockEpochChan,
			timerFactory:   timerFactory,
			sweeper:        sweeper,
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
	signalSwapPaymentResult := ctx.AssertPaid(server.SwapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(server.PrepayInvoiceDesc)

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
	expiryChan <- server.TestTime

	// Now, we notify the height at which the client will start using the
	// default confirmation target. This has the effect of lowering our fees
	// so that the client still start sweeping.
	defaultConfTargetHeight := ctx.Lnd.Height + server.TestLoopOutMinOnChainCltvDelta -
		DefaultSweepConfTargetDelta
	blockEpochChan <- defaultConfTargetHeight

	// This time when we tick the expiry chan, our fees are lower than the
	// swap max, so we expect it to prompt a sweep.
	expiryChan <- server.TestTime

	// Expect a signing request for the HTLC success transaction.
	<-ctx.Lnd.SignOutputRawChannel

	// This is the first time we have swept, so we expect our preimage
	// revealed state to be set.
	cfg.store.(*storeMock).assertLoopOutState(loopdb.StatePreimageRevealed)
	status := <-statusChan
	require.Equal(
		t, status.State, loopdb.StatePreimageRevealed,
	)

	// We expect the sweep tx to have been published.
	ctx.ReceiveTx()

	// Once we have published an on chain sweep, we expect a preimage to
	// have been pushed to the server after the sweep.
	preimage := <-mockServer.PreimagePush
	require.Equal(t, swap.Preimage, preimage)

	// To mock a server failure, we do not send a payment settled update
	// for our off chain payment yet. We also do not confirm our sweep on
	// chain yet so we can test our preimage push retry logic. Instead, we
	// tick the expiry chan again to prompt another sweep.
	expiryChan <- server.TestTime

	// We expect another signing request for out sweep, and publish of our
	// sweep transaction.
	<-ctx.Lnd.SignOutputRawChannel
	ctx.ReceiveTx()

	// Since we have not yet been notified of an off chain settle, and we
	// have attempted to sweep again, we expect another preimage push
	// attempt.
	preimage = <-mockServer.PreimagePush
	require.Equal(t, swap.Preimage, preimage)

	// This time, we send a payment succeeded update into our payment stream
	// to reflect that the server received our preimage push and settled off
	// chain.
	trackPayment.Updates <- lndclient.PaymentStatus{
		State: lnrpc.Payment_SUCCEEDED,
	}

	// We tick one last time, this time expecting a sweep but no preimage
	// push. The test's mocked preimage channel is un-buffered, so our test
	// would hang if we pushed the preimage here.
	expiryChan <- server.TestTime
	<-ctx.Lnd.SignOutputRawChannel
	sweepTx := ctx.ReceiveTx()

	// Finally, we put this swap out of its misery and notify a successful
	// spend our our sweepTx and assert that the swap succeeds.
	ctx.NotifySpend(sweepTx, 0)

	cfg.store.(*storeMock).assertLoopOutState(loopdb.StateSuccess)
	status = <-statusChan
	require.Equal(
		t, status.State, loopdb.StateSuccess,
	)

	require.NoError(t, <-errChan)
}
