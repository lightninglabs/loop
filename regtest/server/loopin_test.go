package server

import (
	"context"
	"crypto/sha256"
	"io"
	"log"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapserverrpc"
	looptest "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestLoopInTermsAndQuote(t *testing.T) {
	t.Parallel()

	_, destination := looptest.CreateKey(30)
	server := &Server{cfg: Config{
		MinSwapAmount:   50_000,
		MaxSwapAmount:   5_000_000,
		FeeBaseSat:      100,
		FeePPM:          1_000,
		LoopInCltvDelta: 100,
	}}

	terms, err := server.LoopInTerms(
		context.Background(), &swapserverrpc.ServerLoopInTermsRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 50_000, terms.MinSwapAmount)
	require.EqualValues(t, 5_000_000, terms.MaxSwapAmount)

	quote, err := server.LoopInQuote(
		context.Background(), &swapserverrpc.ServerLoopInQuoteRequest{
			Amt:             100_000,
			Pubkey:          destination.SerializeCompressed(),
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 200, quote.SwapFee)
	require.EqualValues(t, 100, quote.CltvDelta)

	_, err = server.LoopInTerms(
		context.Background(), &swapserverrpc.ServerLoopInTermsRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_HTLC_V3,
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestLoopInFullFlowProbeAndDuplicate(t *testing.T) {
	lnd := looptest.NewMockLnd()
	lnd.ChainParams = &chaincfg.RegressionNetParams

	serverCtx, cancel := context.WithCancel(context.Background())
	server := &Server{
		cfg: Config{
			Lnd:             &lnd.LndServices,
			MinSwapAmount:   50_000,
			MaxSwapAmount:   5_000_000,
			LoopInCltvDelta: 100,
			FeeBaseSat:      100,
			FeePPM:          1_000,
			PaymentTimeout:  time.Minute,
			Logger:          log.New(io.Discard, "", 0),
		},
		ctx:     serverCtx,
		cancel:  cancel,
		loopIns: make(map[lntypes.Hash]*loopInSwap),
	}

	stopped := false
	t.Cleanup(func() {
		if !stopped {
			server.Stop()
		}
		lnd.WaitForFinished()
	})

	var preimage lntypes.Preimage
	preimage[0] = 1
	hash := preimage.Hash()
	probeHash := lntypes.Hash(sha256.Sum256(hash[:]))
	probeHash[0] ^= 1

	const amount = btcutil.Amount(100_000)
	invoiceAmount := amount - server.swapFee(amount)
	invoiceSigner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	swapInvoice := encodeLoopInTestInvoice(
		t, invoiceSigner, hash, invoiceAmount,
	)
	probeInvoice := encodeLoopInTestInvoice(
		t, invoiceSigner, probeHash, invoiceAmount,
	)
	decodedSwapInvoice, err := zpay32.Decode(
		swapInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	decodedProbeInvoice, err := zpay32.Decode(
		probeInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Equal(
		t, decodedSwapInvoice.Destination.SerializeCompressed(),
		decodedProbeInvoice.Destination.SerializeCompressed(),
	)

	_, senderScriptKey := looptest.CreateKey(31)
	senderInternalPrivKey, senderInternalKey := looptest.CreateKey(32)
	request := &swapserverrpc.ServerLoopInRequest{
		SenderKey:            senderScriptKey.SerializeCompressed(),
		SenderInternalPubkey: senderInternalKey.SerializeCompressed(),
		SwapHash:             hash[:],
		Amt:                  uint64(amount),
		SwapInvoice:          swapInvoice,
		ProtocolVersion:      swapserverrpc.ProtocolVersion_MUSIG2,
		ProbeInvoice:         probeInvoice,
	}

	type loopInResult struct {
		response *swapserverrpc.ServerLoopInResponse
		err      error
	}
	resultChan := make(chan loopInResult, 1)
	go func() {
		response, err := server.NewLoopInSwap(
			context.Background(), request,
		)
		resultChan <- loopInResult{response: response, err: err}
	}()

	// The RPC must remain blocked while the payment is merely in flight,
	// and return only once the canceled hold invoice fails at the sender.
	var probePayment looptest.RouterPaymentChannelMessage
	select {
	case probePayment = <-lnd.RouterSendPaymentChannel:
	case result := <-resultChan:
		t.Fatalf("NewLoopInSwap failed before probing: %v", result.err)
	case <-time.After(looptest.Timeout):
		t.Fatal("router payment was not initiated")
	}
	require.Equal(t, probeInvoice, probePayment.Invoice)
	probePayment.Updates <- lndclient.PaymentStatus{
		State: lnrpc.Payment_IN_FLIGHT,
	}
	select {
	case result := <-resultChan:
		t.Fatalf("NewLoopInSwap returned before probe cancellation: %v",
			result.err)
	case <-time.After(25 * time.Millisecond):
	}

	// A conflicting replay must be rejected immediately even while the
	// original request is still blocked on the probe handshake.
	conflict := cloneLoopInRequest(request)
	conflict.Amt++
	_, err = server.NewLoopInSwap(context.Background(), conflict)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	probePayment.Updates <- lndclient.PaymentStatus{
		State:         lnrpc.Payment_FAILED,
		FailureReason: lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS,
	}

	var first loopInResult
	select {
	case first = <-resultChan:
	case <-time.After(looptest.Timeout):
		t.Fatal("NewLoopInSwap did not finish after probe cancellation")
	}
	require.NoError(t, first.err)
	require.Len(t, first.response.ReceiverKey, 33)
	require.Len(t, first.response.ReceiverInternalPubkey, 33)
	require.EqualValues(t, 700, first.response.Expiry)

	// The background worker must register for the actual P2TR swap script.
	var registration *looptest.ConfRegistration
	select {
	case registration = <-lnd.RegisterConfChannel:
	case <-time.After(looptest.Timeout):
		t.Fatal("Loop In HTLC confirmation was not registered")
	}
	require.Nil(t, registration.TxID)
	require.NotEmpty(t, registration.PkScript)
	require.EqualValues(t, 1, registration.NumConfs)

	server.mu.RLock()
	loopIn := server.loopIns[hash]
	server.mu.RUnlock()
	require.NotNil(t, loopIn)
	updates := loopIn.updates.subscribe()
	defer updates.cancel()

	duplicate, err := server.NewLoopInSwap(
		context.Background(), cloneLoopInRequest(request),
	)
	require.NoError(t, err)
	require.Equal(t, first.response, duplicate)
	select {
	case <-lnd.RouterSendPaymentChannel:
		t.Fatal("duplicate swap started a second probe payment")
	default:
	}

	_, alternateKey := looptest.CreateKey(33)
	_, alternateInternalKey := looptest.CreateKey(34)
	_, alternateLastHop := looptest.CreateKey(35)
	conflicts := []struct {
		name   string
		mutate func(*swapserverrpc.ServerLoopInRequest)
	}{
		{
			name: "amount",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.Amt++
			},
		},
		{
			name: "swap invoice",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.SwapInvoice += "-different"
			},
		},
		{
			name: "probe invoice",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.ProbeInvoice += "-different"
			},
		},
		{
			name: "sender key",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.SenderKey = alternateKey.SerializeCompressed()
			},
		},
		{
			name: "sender internal key",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.SenderInternalPubkey =
					alternateInternalKey.SerializeCompressed()
			},
		},
		{
			name: "last hop",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.LastHop = alternateLastHop.SerializeCompressed()
			},
		},
		{
			name: "protocol",
			mutate: func(req *swapserverrpc.ServerLoopInRequest) {
				req.ProtocolVersion = swapserverrpc.ProtocolVersion_HTLC_V3
			},
		},
	}
	for _, testCase := range conflicts {
		t.Run("conflicting duplicate "+testCase.name, func(t *testing.T) {
			conflictingRequest := cloneLoopInRequest(request)
			testCase.mutate(conflictingRequest)

			_, err := server.NewLoopInSwap(
				context.Background(), conflictingRequest,
			)
			require.Equal(t, codes.AlreadyExists, status.Code(err))
		})
	}

	_, err = server.PushKey(
		context.Background(), &swapserverrpc.ServerPushKeyReq{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			SwapHash:        hash[:],
			InternalPrivkey: senderInternalPrivKey.Serialize(),
		},
	)
	require.NoError(t, err)

	// Confirm an exact-value output to the negotiated P2TR HTLC. This must
	// trigger payment of the real (non-hold) invoice.
	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    int64(amount),
		PkScript: loopIn.htlc.PkScript,
	})
	registration.ConfChan <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: 601,
	}

	var swapPayment looptest.RouterPaymentChannelMessage
	select {
	case swapPayment = <-lnd.RouterSendPaymentChannel:
	case <-time.After(looptest.Timeout):
		t.Fatal("swap invoice payment was not initiated")
	}
	require.Equal(t, swapInvoice, swapPayment.Invoice)
	swapPayment.Updates <- lndclient.PaymentStatus{
		State:    lnrpc.Payment_SUCCEEDED,
		Preimage: preimage,
	}

	var signRequest looptest.SignOutputRawRequest
	select {
	case signRequest = <-lnd.SignOutputRawChannel:
	case <-time.After(looptest.Timeout):
		t.Fatal("success sweep was not signed")
	}
	require.Len(t, signRequest.SignDescriptors, 1)
	require.Equal(
		t, loopIn.htlc.SuccessScript(),
		signRequest.SignDescriptors[0].WitnessScript,
	)

	var sweepRegistration *looptest.ConfRegistration
	select {
	case sweepRegistration = <-lnd.RegisterConfChannel:
	case <-time.After(looptest.Timeout):
		t.Fatal("success sweep confirmation was not registered")
	}
	require.NotNil(t, sweepRegistration.TxID)

	var sweepTx *wire.MsgTx
	select {
	case sweepTx = <-lnd.TxPublishChannel:
	case <-time.After(looptest.Timeout):
		t.Fatal("success sweep was not published")
	}
	require.Equal(t, fundingTx.TxHash(),
		sweepTx.TxIn[0].PreviousOutPoint.Hash)
	require.Equal(t, loopIn.htlc.SuccessSequence(),
		sweepTx.TxIn[0].Sequence)
	require.True(t, loopIn.htlc.IsSuccessWitness(sweepTx.TxIn[0].Witness))
	require.Equal(t, sweepTx.TxHash(), *sweepRegistration.TxID)

	sweepRegistration.ConfChan <- &chainntnfs.TxConfirmation{
		Tx:          sweepTx,
		BlockHeight: 602,
	}

	states := make([]swapserverrpc.ServerSwapState, 0, 3)
	for _, update := range updates.history {
		states = append(states, update.state)
	}
	for len(states) < 3 {
		select {
		case update, ok := <-updates.updates:
			require.True(t, ok)
			states = append(states, update.state)

		case <-time.After(looptest.Timeout):
			t.Fatal("server did not publish all Loop In states")
		}
	}
	require.Equal(t, []swapserverrpc.ServerSwapState{
		swapserverrpc.ServerSwapState_SERVER_INITIATED,
		swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED,
		swapserverrpc.ServerSwapState_SERVER_SUCCESS,
	}, states)

	server.Stop()
	stopped = true
}

func TestProbeLoopInInvoiceRejectsNoRouteAfterInFlight(t *testing.T) {
	t.Parallel()

	lnd := looptest.NewMockLnd()
	server := &Server{cfg: Config{
		Lnd:            &lnd.LndServices,
		MaxSwapAmount:  5_000_000,
		PaymentTimeout: time.Minute,
	}}

	result := make(chan error, 1)
	go func() {
		result <- server.probeLoopInInvoice(
			context.Background(), "probe-invoice", nil,
		)
	}()

	var payment looptest.RouterPaymentChannelMessage
	select {
	case payment = <-lnd.RouterSendPaymentChannel:
	case <-time.After(looptest.Timeout):
		t.Fatal("probe payment was not initiated")
	}
	payment.Updates <- lndclient.PaymentStatus{
		State: lnrpc.Payment_IN_FLIGHT,
	}
	payment.Updates <- lndclient.PaymentStatus{
		State:         lnrpc.Payment_FAILED,
		FailureReason: lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	}

	select {
	case err := <-result:
		require.ErrorContains(t, err, "unexpected failure reason")
	case <-time.After(looptest.Timeout):
		t.Fatal("probe did not return its terminal failure")
	}
}

func TestConfirmedLoopInOutput(t *testing.T) {
	t.Parallel()

	pkScript := []byte{0x51, 0x20, 0x01}
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: pkScript})
	confirmation := &chainntnfs.TxConfirmation{Tx: tx}

	outpoint, amount, state, err := confirmedLoopInOutput(
		confirmation, pkScript, 100_000,
	)
	require.NoError(t, err)
	require.EqualValues(t, 100_000, amount)
	require.Equal(t, tx.TxHash(), outpoint.Hash)
	require.Equal(t,
		swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED, state)

	_, _, state, err = confirmedLoopInOutput(
		confirmation, pkScript, 99_999,
	)
	require.Error(t, err)
	require.Equal(t,
		swapserverrpc.ServerSwapState_SERVER_FAILED_INVALID_HTLC_AMOUNT,
		state,
	)

	tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: pkScript})
	_, _, state, err = confirmedLoopInOutput(
		confirmation, pkScript, 100_000,
	)
	require.Error(t, err)
	require.Equal(t,
		swapserverrpc.ServerSwapState_SERVER_FAILED_MULTIPLE_SWAP_SCRIPTS,
		state,
	)
}

func encodeLoopInTestInvoice(t *testing.T, signer *btcec.PrivateKey,
	hash lntypes.Hash, amount btcutil.Amount) string {

	t.Helper()

	invoice, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, hash, time.Unix(1_700_000_000, 0),
		zpay32.Description("regtest Loop In"),
		zpay32.Amount(lnwire.NewMSatFromSatoshis(amount)),
	)
	require.NoError(t, err)

	encoded, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(message []byte) ([]byte, error) {
			digest := chainhash.HashB(message)
			return ecdsa.SignCompact(signer, digest, true), nil
		},
	})
	require.NoError(t, err)

	return encoded
}

func cloneLoopInRequest(
	req *swapserverrpc.ServerLoopInRequest) *swapserverrpc.ServerLoopInRequest {

	return proto.Clone(req).(*swapserverrpc.ServerLoopInRequest)
}
