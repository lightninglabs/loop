package loop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	testAddr, _ = btcutil.NewAddressScriptHash(
		[]byte{123}, &chaincfg.TestNet3Params,
	)

	testRequest = &OutRequest{
		Amount:              btcutil.Amount(50000),
		DestAddr:            testAddr,
		MaxMinerFee:         50000,
		HtlcConfirmations:   defaultConfirmations,
		SweepConfTarget:     2,
		MaxSwapFee:          1050,
		MaxPrepayAmount:     100,
		MaxPrepayRoutingFee: 75000,
		MaxSwapRoutingFee:   70000,
	}

	swapInvoiceDesc   = "swap"
	prepayInvoiceDesc = "prepay"

	defaultConfirmations = int32(loopdb.DefaultLoopOutHtlcConfirmations)
)

// TestSuccess tests the loop out happy flow, using a custom htlc confirmation
// target.
func TestSuccess(t *testing.T) {
	defer test.Guard(t)()

	ctx := createClientTestContext(t, nil)

	req := *testRequest
	req.HtlcConfirmations = 2

	// Initiate loop out.
	info, err := ctx.swapClient.LoopOut(context.Background(), &req)
	if err != nil {
		t.Fatal(err)
	}

	ctx.assertStored()
	ctx.assertStatus(loopdb.StateInitiated)

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf.
	confIntent := ctx.AssertRegisterConf(false, req.HtlcConfirmations)

	testSuccess(ctx, testRequest.Amount, info.SwapHash,
		signalPrepaymentResult, signalSwapPaymentResult, false,
		confIntent,
	)
}

// TestFailOffchain tests the handling of swap for which the server failed the
// payments.
func TestFailOffchain(t *testing.T) {
	defer test.Guard(t)()

	ctx := createClientTestContext(t, nil)

	_, err := ctx.swapClient.LoopOut(context.Background(), testRequest)
	if err != nil {
		t.Fatal(err)
	}

	ctx.assertStored()
	ctx.assertStatus(loopdb.StateInitiated)

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	ctx.AssertRegisterConf(false, defaultConfirmations)

	signalSwapPaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	signalPrepaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	ctx.assertStatus(loopdb.StateFailOffchainPayments)

	ctx.assertStoreFinished(loopdb.StateFailOffchainPayments)

	ctx.finish()
}

// TestWrongAmount asserts that the client checks the server invoice amounts.
func TestFailWrongAmount(t *testing.T) {
	defer test.Guard(t)()

	test := func(t *testing.T, modifier func(*serverMock),
		expectedErr error) {

		ctx := createClientTestContext(t, nil)

		// Modify mock for this subtest.
		modifier(ctx.serverMock)

		_, err := ctx.swapClient.LoopOut(
			context.Background(), testRequest,
		)
		if err != expectedErr {
			t.Fatalf("Expected %v, but got %v", expectedErr, err)
		}
		ctx.finish()
	}

	t.Run("swap fee too high", func(t *testing.T) {
		test(t, func(m *serverMock) {
			m.swapInvoiceAmt += 10
		}, ErrSwapFeeTooHigh)
	})

	t.Run("prepay amount too high", func(t *testing.T) {
		test(t, func(m *serverMock) {
			// Keep total swap fee unchanged, but increase prepaid
			// portion.
			m.swapInvoiceAmt -= 10
			m.prepayInvoiceAmt += 10
		}, ErrPrepayAmountTooHigh)
	})

}

// TestResume tests that swaps in various states are properly resumed after a
// restart.
func TestResume(t *testing.T) {
	defer test.Guard(t)()

	defaultConfs := loopdb.DefaultLoopOutHtlcConfirmations

	t.Run("not expired", func(t *testing.T) {
		testResume(t, defaultConfs, false, false, true)
	})
	t.Run("not expired, custom confirmations", func(t *testing.T) {
		testResume(t, 3, false, false, true)
	})
	t.Run("expired not revealed", func(t *testing.T) {
		testResume(t, defaultConfs, true, false, false)
	})
	t.Run("expired revealed", func(t *testing.T) {
		testResume(t, defaultConfs, true, true, true)
	})
}

func testResume(t *testing.T, confs uint32, expired, preimageRevealed,
	expectSuccess bool) {

	defer test.Guard(t)()

	preimage := testPreimage
	hash := sha256.Sum256(preimage[:])

	dest := test.GetDestAddr(t, 0)

	amt := btcutil.Amount(50000)

	swapPayReq, err := getInvoice(hash, amt, swapInvoiceDesc)
	if err != nil {
		t.Fatal(err)
	}

	prePayReq, err := getInvoice(hash, 100, prepayInvoiceDesc)
	if err != nil {
		t.Fatal(err)
	}

	_, senderPubKey := test.CreateKey(1)
	var senderKey [33]byte
	copy(senderKey[:], senderPubKey.SerializeCompressed())

	_, receiverPubKey := test.CreateKey(2)
	var receiverKey [33]byte
	copy(receiverKey[:], receiverPubKey.SerializeCompressed())

	update := loopdb.LoopEvent{
		SwapStateData: loopdb.SwapStateData{
			State: loopdb.StateInitiated,
		},
	}

	if preimageRevealed {
		update.State = loopdb.StatePreimageRevealed
		update.HtlcTxHash = &chainhash.Hash{1, 2, 6}
	}

	// Create a pending swap with our custom number of confirmations.
	pendingSwap := &loopdb.LoopOut{
		Contract: &loopdb.LoopOutContract{
			DestAddr:          dest,
			SwapInvoice:       swapPayReq,
			SweepConfTarget:   2,
			HtlcConfirmations: confs,
			MaxSwapRoutingFee: 70000,
			PrepayInvoice:     prePayReq,
			SwapContract: loopdb.SwapContract{
				Preimage:        preimage,
				AmountRequested: amt,
				CltvExpiry:      744,
				ReceiverKey:     receiverKey,
				SenderKey:       senderKey,
				MaxSwapFee:      60000,
				MaxMinerFee:     50000,
			},
		},
		Loop: loopdb.Loop{
			Events: []*loopdb.LoopEvent{&update},
			Hash:   hash,
		},
	}

	if expired {
		// Set cltv expiry so that it has already expired at the test
		// block height.
		pendingSwap.Contract.CltvExpiry = 610
	}

	ctx := createClientTestContext(t, []*loopdb.LoopOut{pendingSwap})

	if preimageRevealed {
		ctx.assertStatus(loopdb.StatePreimageRevealed)
	} else {
		ctx.assertStatus(loopdb.StateInitiated)
	}

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for our expected number of confirmations.
	confIntent := ctx.AssertRegisterConf(preimageRevealed, int32(confs))

	signalSwapPaymentResult(nil)
	signalPrepaymentResult(nil)

	if !expectSuccess {
		ctx.assertStatus(loopdb.StateFailTimeout)
		ctx.assertStoreFinished(loopdb.StateFailTimeout)
		ctx.finish()
		return
	}

	// Because there is no reliable payment yet, an invoice is assumed to be
	// paid after resume.

	testSuccess(ctx, amt, hash,
		func(r error) {},
		func(r error) {},
		preimageRevealed,
		confIntent,
	)
}

func testSuccess(ctx *testContext, amt btcutil.Amount, hash lntypes.Hash,
	signalPrepaymentResult, signalSwapPaymentResult func(error),
	preimageRevealed bool, confIntent *test.ConfRegistration) {

	htlcOutpoint := ctx.publishHtlc(confIntent.PkScript, amt)

	signalPrepaymentResult(nil)

	ctx.AssertRegisterSpendNtfn(confIntent.PkScript)

	// Assert that a call to track payment was sent, and respond with status
	// in flight so that our swap will push its preimage to the server.
	ctx.trackPayment(lnrpc.Payment_IN_FLIGHT)

	// Publish tick.
	ctx.expiryChan <- testTime

	// Expect a signing request.
	<-ctx.Lnd.SignOutputRawChannel

	if !preimageRevealed {
		ctx.assertStatus(loopdb.StatePreimageRevealed)
		ctx.assertStorePreimageReveal()
	}

	// Expect client on-chain sweep of HTLC.
	sweepTx := ctx.ReceiveTx()

	if !bytes.Equal(sweepTx.TxIn[0].PreviousOutPoint.Hash[:],
		htlcOutpoint.Hash[:]) {
		ctx.T.Fatalf("client not sweeping from htlc tx")
	}

	// Check preimage.
	clientPreImage := sweepTx.TxIn[0].Witness[1]
	clientPreImageHash := sha256.Sum256(clientPreImage)
	if clientPreImageHash != hash {
		ctx.T.Fatalf("incorrect preimage")
	}

	// Since we successfully published our sweep, we expect the preimage to
	// have been pushed to our mock server.
	preimage, err := lntypes.MakePreimage(clientPreImage)
	require.NoError(ctx.T, err)

	ctx.assertPreimagePush(preimage)

	// Simulate server pulling payment.
	signalSwapPaymentResult(nil)

	ctx.NotifySpend(sweepTx, 0)

	ctx.assertStatus(loopdb.StateSuccess)

	ctx.assertStoreFinished(loopdb.StateSuccess)

	ctx.finish()
}
