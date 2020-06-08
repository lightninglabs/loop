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
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
)

// TestLoopOutPaymentParameters tests the first part of the loop out process up
// to the point where the off-chain payments are made.
func TestLoopOutPaymentParameters(t *testing.T) {
	defer test.Guard(t)()

	// Set up test context objects.
	lnd := test.NewMockLnd()
	ctx := test.NewContext(t, lnd)
	server := newServerMock()
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

	const maxParts = 5

	// Initiate the swap.
	req := *testRequest
	req.OutgoingChanSet = loopdb.ChannelSet{2, 3}

	swap, err := newLoopOutSwap(
		context.Background(), cfg, height, &req,
	)
	if err != nil {
		t.Fatal(err)
	}

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
	ctx.AssertRegisterConf()

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

	server := newServerMock()

	store := newStoreMock(t)

	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	height := int32(600)

	cfg := newSwapConfig(&lnd.LndServices, store, server)

	swap, err := newLoopOutSwap(
		context.Background(), cfg, height, testRequest,
	)
	if err != nil {
		t.Fatal(err)
	}

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

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf
	ctx.AssertRegisterConf()

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

	// Use the highest sweep confirmation target before we attempt to use
	// the default.
	testRequest.SweepConfTarget = testLoopOutOnChainCltvDelta -
		DefaultSweepConfTargetDelta - 1

	// Set up custom fee estimates such that the lower confirmation target
	// yields a much higher fee rate.
	ctx.Lnd.SetFeeEstimate(testRequest.SweepConfTarget, 250)
	ctx.Lnd.SetFeeEstimate(DefaultSweepConfTarget, 10000)

	cfg := newSwapConfig(
		&lnd.LndServices, newStoreMock(t), newServerMock(),
	)

	swap, err := newLoopOutSwap(
		context.Background(), cfg, ctx.Lnd.Height, testRequest,
	)
	if err != nil {
		t.Fatal(err)
	}

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
	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	signalSwapPaymentResult(nil)
	signalPrepaymentResult(nil)

	// Notify the confirmation notification for the HTLC.
	ctx.AssertRegisterConf()

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
	_ = assertSweepTx(testRequest.SweepConfTarget)

	// We'll then notify the height at which we begin using the default
	// confirmation target.
	defaultConfTargetHeight := ctx.Lnd.Height + testLoopOutOnChainCltvDelta -
		DefaultSweepConfTargetDelta
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
