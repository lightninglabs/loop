package loop

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		Initiator:           "test",
	}

	swapInvoiceDesc   = "swap"
	prepayInvoiceDesc = "prepay"

	defaultConfirmations = int32(loopdb.DefaultLoopOutHtlcConfirmations)
)

// TestLoopOutSuccess tests the loop out happy flow, using a custom htlc
// confirmation target.
func TestLoopOutSuccess(t *testing.T) {
	defer test.Guard(t)()

	ctx := createClientTestContext(t, nil)

	req := *testRequest
	req.HtlcConfirmations = 2

	// Initiate loop out.
	info, err := ctx.swapClient.LoopOut(context.Background(), &req)
	require.NoError(t, err)

	ctx.assertStored()
	ctx.assertStatus(loopdb.StateInitiated)

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf.
	confIntent := ctx.AssertRegisterConf(false, req.HtlcConfirmations)

	testLoopOutSuccess(ctx, testRequest.Amount, info.SwapHash,
		signalPrepaymentResult, signalSwapPaymentResult, false,
		confIntent, swap.HtlcV2,
	)
}

// TestLoopOutFailOffchain tests the handling of swap for which the server
// failed the payments.
func TestLoopOutFailOffchain(t *testing.T) {
	defer test.Guard(t)()

	ctx := createClientTestContext(t, nil)

	_, err := ctx.swapClient.LoopOut(context.Background(), testRequest)
	require.NoError(t, err)

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
	<-ctx.serverMock.cancelSwap
	ctx.assertStatus(loopdb.StateFailOffchainPayments)

	ctx.assertStoreFinished(loopdb.StateFailOffchainPayments)

	ctx.finish()
}

// TestLoopOutWrongAmount asserts that the client checks the server invoice
// amounts.
func TestLoopOutFailWrongAmount(t *testing.T) {
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

// TestLoopOutResume tests that swaps in various states are properly resumed
// after a restart.
func TestLoopOutResume(t *testing.T) {
	defer test.Guard(t)()

	defaultConfs := loopdb.DefaultLoopOutHtlcConfirmations

	storedVersion := []loopdb.ProtocolVersion{
		loopdb.ProtocolVersionUnrecorded,
		loopdb.ProtocolVersionHtlcV2,
		loopdb.ProtocolVersionHtlcV3,
	}

	for _, version := range storedVersion {
		version := version

		t.Run(version.String(), func(t *testing.T) {
			t.Run("not expired", func(t *testing.T) {
				testLoopOutResume(
					t, defaultConfs, false, false, true,
					version,
				)
			})
			t.Run("not expired, custom confirmations",
				func(t *testing.T) {
					testLoopOutResume(
						t, 3, false, false, true,
						version,
					)
				})
			t.Run("expired not revealed", func(t *testing.T) {
				testLoopOutResume(
					t, defaultConfs, true, false, false,
					version,
				)
			})
			t.Run("expired revealed", func(t *testing.T) {
				testLoopOutResume(
					t, defaultConfs, true, true, true,
					version,
				)
			})
		})
	}
}

func testLoopOutResume(t *testing.T, confs uint32, expired, preimageRevealed,
	expectSuccess bool, protocolVersion loopdb.ProtocolVersion) {

	defer test.Guard(t)()

	preimage := testPreimage
	hash := sha256.Sum256(preimage[:])

	dest := test.GetDestAddr(t, 0)

	amt := btcutil.Amount(50000)

	swapPayReq, err := getInvoice(hash, amt, swapInvoiceDesc)
	require.NoError(t, err)

	prePayReq, err := getInvoice(hash, 100, prepayInvoiceDesc)
	require.NoError(t, err)

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

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for our expected number of confirmations.
	confIntent := ctx.AssertRegisterConf(preimageRevealed, int32(confs))

	// Assert that the loopout htlc equals to the expected one.
	scriptVersion := GetHtlcScriptVersion(protocolVersion)
	var htlc *swap.Htlc

	switch scriptVersion {
	case swap.HtlcV2:
		htlc, err = swap.NewHtlcV2(
			pendingSwap.Contract.CltvExpiry, senderKey,
			receiverKey, hash, &chaincfg.TestNet3Params,
		)

	case swap.HtlcV3:
		htlc, err = swap.NewHtlcV3(
			input.MuSig2Version040,
			pendingSwap.Contract.CltvExpiry, senderKey,
			receiverKey, senderKey, receiverKey, hash,
			&chaincfg.TestNet3Params,
		)

	default:
		t.Fatalf(swap.ErrInvalidScriptVersion.Error())
	}

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
	testLoopOutSuccess(ctx, amt, hash,
		func(r error) {},
		func(r error) {},
		preimageRevealed,
		confIntent, scriptVersion,
	)
}

func testLoopOutSuccess(ctx *testContext, amt btcutil.Amount, hash lntypes.Hash,
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
	ctx.expiryChan <- testTime

	// Expect a signing request in the non taproot case.
	if scriptVersion != swap.HtlcV3 {
		<-ctx.Lnd.SignOutputRawChannel
	}

	if !preimageRevealed {
		ctx.assertStatus(loopdb.StatePreimageRevealed)
		ctx.assertStorePreimageReveal()
	}

	// When using taproot htlcs the flow is different as we do reveal the
	// preimage before sweeping in order for the server to trust us with
	// our MuSig2 signing attempts.
	if scriptVersion == swap.HtlcV3 {
		ctx.assertPreimagePush(testPreimage)

		// Try MuSig2 signing first and fail it so that we go for a
		// normal sweep.
		for i := 0; i < maxMusigSweepRetries; i++ {
			ctx.expiryChan <- testTime
			ctx.assertPreimagePush(testPreimage)
		}
		<-ctx.Lnd.SignOutputRawChannel
	}

	// Expect client on-chain sweep of HTLC.
	sweepTx := ctx.ReceiveTx()

	require.Equal(
		ctx.T, htlcOutpoint.Hash[:],
		sweepTx.TxIn[0].PreviousOutPoint.Hash[:],
		"client not sweeping from htlc tx",
	)

	var preImageIndex int
	switch scriptVersion {
	case swap.HtlcV2:
		preImageIndex = 0

	case swap.HtlcV3:
		preImageIndex = 0
	}

	// Check preimage.
	clientPreImage := sweepTx.TxIn[0].Witness[preImageIndex]
	clientPreImageHash := sha256.Sum256(clientPreImage)
	require.Equal(ctx.T, hash, lntypes.Hash(clientPreImageHash))

	// Since we successfully published our sweep, we expect the preimage to
	// have been pushed to our mock server.
	preimage, err := lntypes.MakePreimage(clientPreImage)
	require.NoError(ctx.T, err)

	if scriptVersion != swap.HtlcV3 {
		ctx.assertPreimagePush(preimage)
		// Simulate server pulling payment.
		signalSwapPaymentResult(nil)
	}

	ctx.NotifySpend(sweepTx, 0)

	ctx.assertStatus(loopdb.StateSuccess)

	ctx.assertStoreFinished(loopdb.StateSuccess)

	ctx.finish()
}

// TestWrapGrpcError tests grpc error wrapping in the case where a grpc error
// code is present, and when it is absent.
func TestWrapGrpcError(t *testing.T) {
	tests := []struct {
		name         string
		original     error
		expectedCode codes.Code
	}{
		{
			name: "out of range error",
			original: status.Error(
				codes.OutOfRange, "err string",
			),
			expectedCode: codes.OutOfRange,
		},
		{
			name:         "no grpc code",
			original:     errors.New("no error code"),
			expectedCode: codes.Unknown,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			err := wrapGrpcError("", testCase.original)
			require.Error(t, err, "test only expects errors")

			status, ok := status.FromError(err)
			require.True(t, ok, "test expects grpc code")
			require.Equal(t, testCase.expectedCode, status.Code())
		})
	}
}
