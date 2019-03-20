package loop

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	testLoopInRequest = LoopInRequest{
		MaxPrepayAmount: btcutil.Amount(100),
		Amount:          btcutil.Amount(50000),
		MaxSwapFee:      btcutil.Amount(1000),
		HtlcConfTarget:  2,
	}
)

func TestLoopInSuccess(t *testing.T) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg := &swapConfig{
		lnd:    &ctx.lnd.LndServices,
		store:  ctx.store,
		server: ctx.server,
	}

	swap, err := newLoopInSwap(
		context.Background(), cfg,
		height, &testLoopInRequest,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx.store.assertLoopInStored()

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			logger.Error(err)
		}
		errChan <- err
	}()

	ctx.assertState(loopdb.StateInitiated)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.assertLoopInState(loopdb.StateHtlcPublished)

	// Expect htlc to be published.
	htlcTx := <-ctx.lnd.SendOutputsChannel

	// Expect register for htlc conf.
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Expect prepay to be paid.
	prepay := <-ctx.lnd.SendPaymentChannel

	prepayInvoice, err := ctx.lnd.DecodeInvoice(prepay.PaymentRequest)
	if err != nil {
		t.Fatal(err)
	}

	logger.Infof("Prepay hash: %x", *prepayInvoice.PaymentHash)

	// Signal prepay pulled.
	prepay.Done <- nil

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

	// Server spends htlc.
	successTx := wire.MsgTx{}
	successTx.AddTxIn(&wire.TxIn{
		// PreviousOutPoint: wire.OutPoint{
		// 	Hash:  htlcTx.TxHash(),
		// 	Index: uint32(htlcIndex),
		// },
		Witness: [][]byte{{}, {}, {}},
	})

	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        &successTx,
		SpenderInputIndex: 0,
	}

	// Client starts listening for swap invoice updates.
	subscription := <-ctx.lnd.SingleInvoiceSubcribeChannel
	if subscription.Hash != ctx.server.swapHash {
		t.Fatal("client subscribing to wrong invoice")
	}

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	subscription.Update <- channeldb.ContractSettled

	ctx.assertState(loopdb.StateSuccess)
	ctx.store.assertLoopInState(loopdb.StateSuccess)

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoopInTimeout(t *testing.T) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg := &swapConfig{
		lnd:    &ctx.lnd.LndServices,
		store:  ctx.store,
		server: ctx.server,
	}

	swap, err := newLoopInSwap(
		context.Background(), cfg,
		height, &testLoopInRequest,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx.store.assertLoopInStored()

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			logger.Error(err)
		}
		errChan <- err
	}()

	ctx.assertState(loopdb.StateInitiated)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.assertLoopInState(loopdb.StateHtlcPublished)

	// Expect htlc to be published.
	htlcTx := <-ctx.lnd.SendOutputsChannel

	// Expect register for htlc conf.
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Expect prepay to be paid.
	prepay := <-ctx.lnd.SendPaymentChannel

	prepayInvoice, err := ctx.lnd.DecodeInvoice(prepay.PaymentRequest)
	if err != nil {
		t.Fatal(err)
	}

	logger.Infof("Prepay hash: %x", *prepayInvoice.PaymentHash)

	// Signal prepay pulled.
	prepay.Done <- nil

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

	// Let htlc expire.
	ctx.blockEpochChan <- swap.LoopInContract.CltvExpiry

	// Expect timeout tx to be published.
	timeoutTx := <-ctx.lnd.TxPublishChannel

	// Confirm timeout tx.
	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        timeoutTx,
		SpenderInputIndex: 0,
	}

	ctx.assertState(loopdb.StateFailTimeout)
	ctx.store.assertLoopInState(loopdb.StateFailTimeout)

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}

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

	cfg := &swapConfig{
		lnd:    &ctx.lnd.LndServices,
		store:  ctx.store,
		server: ctx.server,
	}

	senderKey := [33]byte{4}
	receiverKey := [33]byte{5}

	prepayInvoice, err := getInvoice(testPreimage.Hash(), 5000, "prepay")
	if err != nil {
		t.Fatal(err)
	}
	contract := &loopdb.LoopInContract{
		HtlcConfTarget: 2,
		SwapContract: loopdb.SwapContract{
			Preimage:        testPreimage,
			AmountRequested: 100000,
			CltvExpiry:      744,
			ReceiverKey:     receiverKey,
			SenderKey:       senderKey,
			MaxSwapFee:      60000,
			PrepayInvoice:   prepayInvoice,
			MaxMinerFee:     50000,
		},
	}
	pendSwap := &loopdb.LoopIn{
		Contract: contract,
		Loop: loopdb.Loop{
			Events: []*loopdb.LoopEvent{
				{
					State: state,
				},
			},
			Hash: testPreimage.Hash(),
		},
	}

	htlc, err := swap.NewHtlc(
		contract.CltvExpiry, contract.SenderKey, contract.ReceiverKey,
		testPreimage.Hash(),
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
			logger.Error(err)
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
	} else {
		ctx.assertState(loopdb.StateHtlcPublished)

		htlcTx.AddTxOut(&wire.TxOut{
			PkScript: htlc.ScriptHash,
		})
	}

	// Expect register for htlc conf.
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Expect prepay to be paid.
	prepay := <-ctx.lnd.SendPaymentChannel

	// Signal prepay pulled.
	prepay.Done <- nil

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

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

	// Client starts listening for swap invoice updates.
	subscription := <-ctx.lnd.SingleInvoiceSubcribeChannel
	if subscription.Hash != testPreimage.Hash() {
		t.Fatal("client subscribing to wrong invoice")
	}

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	subscription.Update <- channeldb.ContractSettled

	ctx.assertState(loopdb.StateSuccess)
}
