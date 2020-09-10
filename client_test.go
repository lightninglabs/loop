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
	"github.com/lightninglabs/loop/server"
	"github.com/lightninglabs/loop/swap"
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

	signalSwapPaymentResult := ctx.AssertPaid(server.SwapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(server.PrepayInvoiceDesc)

	// Expect client to register for conf.
	confIntent := ctx.AssertRegisterConf(false, req.HtlcConfirmations)

	testSuccess(ctx, testRequest.Amount, info.SwapHash,
		signalPrepaymentResult, signalSwapPaymentResult, false,
		confIntent, swap.HtlcV2,
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

	signalSwapPaymentResult := ctx.AssertPaid(server.SwapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(server.PrepayInvoiceDesc)

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

	test := func(t *testing.T, modifier func(*server.Mock),
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
		test(t, func(m *server.Mock) {
			m.SwapInvoiceAmt += 10
		}, ErrSwapFeeTooHigh)
	})

	t.Run("prepay amount too high", func(t *testing.T) {
		test(t, func(m *server.Mock) {
			// Keep total swap fee unchanged, but increase prepaid
			// portion.
			m.SwapInvoiceAmt -= 10
			m.PrepayInvoiceAmt += 10
		}, ErrPrepayAmountTooHigh)
	})

}

// TestResume tests that swaps in various states are properly resumed after a
// restart.
func TestResume(t *testing.T) {
	defer test.Guard(t)()

	defaultConfs := loopdb.DefaultLoopOutHtlcConfirmations

	storedVersion := []loopdb.ProtocolVersion{
		loopdb.ProtocolVersionUnrecorded,
		loopdb.ProtocolVersionHtlcV2,
	}

	for _, version := range storedVersion {
		version := version

		t.Run(version.String(), func(t *testing.T) {
			t.Run("not expired", func(t *testing.T) {
				testResume(
					t, defaultConfs, false, false, true,
					version,
				)
			})
			t.Run("not expired, custom confirmations",
				func(t *testing.T) {
					testResume(
						t, 3, false, false, true,
						version,
					)
				})
			t.Run("expired not revealed", func(t *testing.T) {
				testResume(
					t, defaultConfs, true, false, false,
					version,
				)
			})
			t.Run("expired revealed", func(t *testing.T) {
				testResume(
					t, defaultConfs, true, true, true,
					version,
				)
			})
		})
	}
}

func testResume(t *testing.T, confs uint32, expired, preimageRevealed,
	expectSuccess bool, protocolVersion loopdb.ProtocolVersion) {

	defer test.Guard(t)()

	preimage := testPreimage
	hash := sha256.Sum256(preimage[:])

	dest := test.GetDestAddr(t, 0)

	amt := btcutil.Amount(50000)

	swapPayReq, err := server.GetInvoice(
		hash, amt, server.SwapInvoiceDesc,
	)
	if err != nil {
		t.Fatal(err)
	}

	prePayReq, err := server.GetInvoice(
		hash, 100, server.PrepayInvoiceDesc,
	)
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
				ProtocolVersion: protocolVersion,
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

	signalSwapPaymentResult := ctx.AssertPaid(server.SwapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(server.PrepayInvoiceDesc)

	// Expect client to register for our expected number of confirmations.
	confIntent := ctx.AssertRegisterConf(preimageRevealed, int32(confs))

	// Assert that the loopout htlc equals to the expected one.
	scriptVersion := GetHtlcScriptVersion(protocolVersion)
	htlc, err := swap.NewHtlc(
		scriptVersion, pendingSwap.Contract.CltvExpiry, senderKey,
		receiverKey, hash, swap.HtlcP2WSH, &chaincfg.TestNet3Params,
	)
	require.NoError(t, err)
	require.Equal(t, htlc.PkScript, confIntent.PkScript)

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
		confIntent, scriptVersion,
	)
}

func testSuccess(ctx *testContext, amt btcutil.Amount, hash lntypes.Hash,
	signalPrepaymentResult, signalSwapPaymentResult func(error),
	preimageRevealed bool, confIntent *test.ConfRegistration,
	scriptVersion swap.ScriptVersion) {

	htlcOutpoint := ctx.publishHtlc(confIntent.PkScript, amt)

	signalPrepaymentResult(nil)

	ctx.AssertRegisterSpendNtfn(confIntent.PkScript)

	// Assert that a call to track payment was sent, and respond with status
	// in flight so that our swap will push its preimage to the server.
	ctx.trackPayment(lnrpc.Payment_IN_FLIGHT)

	// Publish tick.
	ctx.expiryChan <- server.TestTime

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

	preImageIndex := 1
	if scriptVersion == swap.HtlcV2 {
		preImageIndex = 0
	}

	// Check preimage.
	clientPreImage := sweepTx.TxIn[0].Witness[preImageIndex]
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
