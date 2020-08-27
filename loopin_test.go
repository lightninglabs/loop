package loop

import (
	"context"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	testLoopInRequest = LoopInRequest{
		Amount:         btcutil.Amount(50000),
		MaxSwapFee:     btcutil.Amount(1000),
		HtlcConfTarget: 2,
	}
)

// TestLoopInSuccess tests the success scenario where the swap completes the
// happy flow.
func TestLoopInSuccess(t *testing.T) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg := newSwapConfig(&ctx.lnd.LndServices, ctx.store, ctx.server)

	initResult, err := newLoopInSwap(
		context.Background(), cfg,
		height, &testLoopInRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	swap := initResult.swap

	ctx.store.assertLoopInStored()

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	ctx.assertState(loopdb.StateInitiated)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.assertLoopInState(loopdb.StateHtlcPublished)

	// Expect htlc to be published.
	htlcTx := <-ctx.lnd.SendOutputsChannel

	// Expect the same state to be written again with the htlc tx hash.
	state := ctx.store.assertLoopInState(loopdb.StateHtlcPublished)
	require.NotNil(t, state.HtlcTxHash)

	// Expect register for htlc conf.
	<-ctx.lnd.RegisterConfChannel
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

	// Client starts listening for swap invoice updates.
	subscription := <-ctx.lnd.SingleInvoiceSubcribeChannel
	if subscription.Hash != ctx.server.swapHash {
		t.Fatal("client subscribing to wrong invoice")
	}

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	subscription.Update <- lndclient.InvoiceUpdate{
		State:   channeldb.ContractSettled,
		AmtPaid: 49000,
	}

	// Swap is expected to move to the state InvoiceSettled
	ctx.assertState(loopdb.StateInvoiceSettled)
	ctx.store.assertLoopInState(loopdb.StateInvoiceSettled)

	// Server spends htlc.
	successTx := wire.MsgTx{}
	successTx.AddTxIn(&wire.TxIn{
		Witness: [][]byte{{}, {}, {}},
	})

	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        &successTx,
		SpenderInputIndex: 0,
	}

	ctx.assertState(loopdb.StateSuccess)
	ctx.store.assertLoopInState(loopdb.StateSuccess)

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}

// TestLoopInTimeout tests scenarios where the server doesn't sweep the htlc
// and the client is forced to reclaim the funds using the timeout tx.
func TestLoopInTimeout(t *testing.T) {
	testAmt := int64(testLoopInRequest.Amount)
	t.Run("internal htlc", func(t *testing.T) {
		testLoopInTimeout(t, swap.HtlcP2WSH, 0)
	})

	outputTypes := []swap.HtlcOutputType{swap.HtlcP2WSH, swap.HtlcNP2WSH}

	for _, outputType := range outputTypes {
		outputType := outputType
		t.Run(outputType.String(), func(t *testing.T) {
			t.Run("external htlc", func(t *testing.T) {
				testLoopInTimeout(t, outputType, testAmt)
			})

			t.Run("external amount too high", func(t *testing.T) {
				testLoopInTimeout(t, outputType, testAmt+1)
			})

			t.Run("external amount too low", func(t *testing.T) {
				testLoopInTimeout(t, outputType, testAmt-1)
			})
		})
	}
}

func testLoopInTimeout(t *testing.T,
	outputType swap.HtlcOutputType, externalValue int64) {

	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg := newSwapConfig(&ctx.lnd.LndServices, ctx.store, ctx.server)

	req := testLoopInRequest
	if externalValue != 0 {
		req.ExternalHtlc = true
	}

	initResult, err := newLoopInSwap(
		context.Background(), cfg,
		height, &req,
	)
	if err != nil {
		t.Fatal(err)
	}
	s := initResult.swap

	ctx.store.assertLoopInStored()

	errChan := make(chan error)
	go func() {
		err := s.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	ctx.assertState(loopdb.StateInitiated)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.assertLoopInState(loopdb.StateHtlcPublished)

	var htlcTx wire.MsgTx
	if externalValue == 0 {
		// Expect htlc to be published.
		htlcTx = <-ctx.lnd.SendOutputsChannel

		// Expect the same state to be written again with the htlc tx hash.
		state := ctx.store.assertLoopInState(loopdb.StateHtlcPublished)
		require.NotNil(t, state.HtlcTxHash)
	} else {
		// Create an external htlc publish tx.
		var pkScript []byte
		if outputType == swap.HtlcNP2WSH {
			pkScript = s.htlcNP2WSH.PkScript
		} else {
			pkScript = s.htlcP2WSH.PkScript
		}
		htlcTx = wire.MsgTx{
			TxOut: []*wire.TxOut{
				{
					PkScript: pkScript,
					Value:    externalValue,
				},
			},
		}
	}

	// Expect register for htlc conf.
	<-ctx.lnd.RegisterConfChannel
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Assert that the swap is failed in case of an invalid amount.
	invalidAmt := externalValue != 0 && externalValue != int64(req.Amount)
	if invalidAmt {
		ctx.assertState(loopdb.StateFailIncorrectHtlcAmt)
		ctx.store.assertLoopInState(loopdb.StateFailIncorrectHtlcAmt)

		err = <-errChan
		if err != nil {
			t.Fatal(err)
		}

		return
	}

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

	// Client starts listening for swap invoice updates.
	subscription := <-ctx.lnd.SingleInvoiceSubcribeChannel
	if subscription.Hash != ctx.server.swapHash {
		t.Fatal("client subscribing to wrong invoice")
	}

	// Let htlc expire.
	ctx.blockEpochChan <- s.LoopInContract.CltvExpiry

	// Expect a signing request for the htlc tx output value.
	signReq := <-ctx.lnd.SignOutputRawChannel
	if signReq.SignDescriptors[0].Output.Value != htlcTx.TxOut[0].Value {

		t.Fatal("invalid signing amount")
	}

	// Expect timeout tx to be published.
	timeoutTx := <-ctx.lnd.TxPublishChannel

	// Confirm timeout tx.
	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        timeoutTx,
		SpenderInputIndex: 0,
	}

	// Now that timeout tx has confirmed, the client should be able to
	// safely cancel the swap invoice.
	<-ctx.lnd.FailInvoiceChannel

	// Signal the the invoice was canceled.
	subscription.Update <- lndclient.InvoiceUpdate{
		State: channeldb.ContractCanceled,
	}

	ctx.assertState(loopdb.StateFailTimeout)
	ctx.store.assertLoopInState(loopdb.StateFailTimeout)

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}

// TestLoopInResume tests resuming swaps in various states.
func TestLoopInResume(t *testing.T) {
	t.Run("initiated", func(t *testing.T) {
		testLoopInResume(t, loopdb.StateInitiated, false)
	})

	t.Run("initiated expired", func(t *testing.T) {
		testLoopInResume(t, loopdb.StateInitiated, true)
	})

	t.Run("htlc published", func(t *testing.T) {
		testLoopInResume(t, loopdb.StateHtlcPublished, false)
	})
}

func testLoopInResume(t *testing.T, state loopdb.SwapState, expired bool) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)
	cfg := newSwapConfig(&ctx.lnd.LndServices, ctx.store, ctx.server)

	senderKey := [33]byte{4}
	receiverKey := [33]byte{5}

	contract := &loopdb.LoopInContract{
		HtlcConfTarget: 2,
		SwapContract: loopdb.SwapContract{
			Preimage:        testPreimage,
			AmountRequested: 100000,
			CltvExpiry:      744,
			ReceiverKey:     receiverKey,
			SenderKey:       senderKey,
			MaxSwapFee:      60000,
			MaxMinerFee:     50000,
		},
	}
	pendSwap := &loopdb.LoopIn{
		Contract: contract,
		Loop: loopdb.Loop{
			Events: []*loopdb.LoopEvent{
				{
					SwapStateData: loopdb.SwapStateData{
						State: state,
					},
				},
			},
			Hash: testPreimage.Hash(),
		},
	}

	htlc, err := swap.NewHtlc(
		swap.HtlcV1, contract.CltvExpiry, contract.SenderKey,
		contract.ReceiverKey, testPreimage.Hash(), swap.HtlcNP2WSH,
		cfg.lnd.ChainParams,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = ctx.store.CreateLoopIn(testPreimage.Hash(), contract)
	if err != nil {
		t.Fatal(err)
	}

	swap, err := resumeLoopInSwap(
		context.Background(), cfg,
		pendSwap,
	)
	if err != nil {
		t.Fatal(err)
	}

	var height int32
	if expired {
		height = 740
	} else {
		height = 600
	}

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	defer func() {
		err = <-errChan
		if err != nil {
			t.Fatal(err)
		}

		select {
		case <-ctx.lnd.SendPaymentChannel:
			t.Fatal("unexpected payment sent")
		default:
		}

		select {
		case <-ctx.lnd.SendOutputsChannel:
			t.Fatal("unexpected tx published")
		default:
		}
	}()

	var htlcTx wire.MsgTx
	if state == loopdb.StateInitiated {
		ctx.assertState(loopdb.StateInitiated)

		if expired {
			ctx.assertState(loopdb.StateFailTimeout)
			return
		}

		ctx.assertState(loopdb.StateHtlcPublished)
		ctx.store.assertLoopInState(loopdb.StateHtlcPublished)

		// Expect htlc to be published.
		htlcTx = <-ctx.lnd.SendOutputsChannel

		// Expect the same state to be written again with the htlc tx
		// hash.
		state := ctx.store.assertLoopInState(loopdb.StateHtlcPublished)
		require.NotNil(t, state.HtlcTxHash)
	} else {
		ctx.assertState(loopdb.StateHtlcPublished)

		htlcTx.AddTxOut(&wire.TxOut{
			PkScript: htlc.PkScript,
			Value:    int64(contract.AmountRequested),
		})
	}

	// Expect register for htlc conf.
	<-ctx.lnd.RegisterConfChannel
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

	// Client starts listening for swap invoice updates.
	subscription := <-ctx.lnd.SingleInvoiceSubcribeChannel
	if subscription.Hash != testPreimage.Hash() {
		t.Fatal("client subscribing to wrong invoice")
	}

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	subscription.Update <- lndclient.InvoiceUpdate{
		State:   channeldb.ContractSettled,
		AmtPaid: 49000,
	}

	// Swap is expected to move to the state InvoiceSettled
	ctx.assertState(loopdb.StateInvoiceSettled)
	ctx.store.assertLoopInState(loopdb.StateInvoiceSettled)

	// Server spends htlc.
	successTx := wire.MsgTx{}
	successTx.AddTxIn(&wire.TxIn{
		Witness: [][]byte{{}, {}, {}},
	})
	successTxHash := successTx.TxHash()

	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        &successTx,
		SpenderTxHash:     &successTxHash,
		SpenderInputIndex: 0,
	}

	ctx.assertState(loopdb.StateSuccess)
}
