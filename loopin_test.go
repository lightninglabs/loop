package loop

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

var (
	testLoopInRequest = LoopInRequest{
		Amount:         btcutil.Amount(50000),
		MaxSwapFee:     btcutil.Amount(1000),
		HtlcConfTarget: 2,
		Initiator:      "test",
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

	// We expect our cost to use the mock fee rate we set for our conf
	// target.
	cost := loopdb.SwapCost{
		Onchain: getTxFee(&htlcTx, test.DefaultMockFee.FeePerKVByte()),
	}

	// Expect the same state to be written again with the htlc tx hash
	// and on chain fee.
	state := ctx.store.assertLoopInState(loopdb.StateHtlcPublished)
	require.NotNil(t, state.HtlcTxHash)
	require.Equal(t, cost, state.Cost)

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
	ctx.assertSubscribeInvoice(ctx.server.swapHash)

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	ctx.updateInvoiceState(49000, channeldb.ContractSettled)

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

	var (
		htlcTx wire.MsgTx
		cost   loopdb.SwapCost
	)
	if externalValue == 0 {
		// Expect htlc to be published.
		htlcTx = <-ctx.lnd.SendOutputsChannel
		cost = loopdb.SwapCost{
			Onchain: getTxFee(
				&htlcTx, test.DefaultMockFee.FeePerKVByte(),
			),
		}

		// Expect the same state to be written again with the htlc tx
		// hash and cost.
		state := ctx.store.assertLoopInState(loopdb.StateHtlcPublished)
		require.NotNil(t, state.HtlcTxHash)
		require.Equal(t, cost, state.Cost)
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
	ctx.assertSubscribeInvoice(ctx.server.swapHash)

	// Let htlc expire.
	ctx.blockEpochChan <- s.LoopInContract.CltvExpiry

	// Expect a signing request for the htlc tx output value.
	signReq := <-ctx.lnd.SignOutputRawChannel
	if signReq.SignDescriptors[0].Output.Value != htlcTx.TxOut[0].Value {

		t.Fatal("invalid signing amount")
	}

	// Expect timeout tx to be published.
	timeoutTx := <-ctx.lnd.TxPublishChannel

	// We can just get our sweep fee as we would in the swap code because
	// our estimate is static.
	fee, err := s.sweeper.GetSweepFee(
		context.Background(), s.htlc.AddTimeoutToEstimator,
		s.timeoutAddr, TimeoutTxConfTarget,
	)
	require.NoError(t, err)
	cost.Onchain += fee

	// Confirm timeout tx.
	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        timeoutTx,
		SpenderInputIndex: 0,
	}

	// Now that timeout tx has confirmed, the client should be able to
	// safely cancel the swap invoice.
	<-ctx.lnd.FailInvoiceChannel

	// Signal that the invoice was canceled.
	ctx.updateInvoiceState(0, channeldb.ContractCanceled)

	ctx.assertState(loopdb.StateFailTimeout)
	state := ctx.store.assertLoopInState(loopdb.StateFailTimeout)
	require.Equal(t, cost, state.Cost)

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}

// TestLoopInResume tests resuming swaps in various states.
func TestLoopInResume(t *testing.T) {
	storedVersion := []loopdb.ProtocolVersion{
		loopdb.ProtocolVersionUnrecorded,
		loopdb.ProtocolVersionHtlcV2,
	}

	htlcVersion := []swap.ScriptVersion{
		swap.HtlcV1,
		swap.HtlcV2,
	}

	for i, version := range storedVersion {
		version := version
		scriptVersion := htlcVersion[i]

		t.Run(version.String(), func(t *testing.T) {
			t.Run("initiated", func(t *testing.T) {
				testLoopInResume(
					t, loopdb.StateInitiated, false,
					version, scriptVersion,
				)
			})

			t.Run("initiated expired", func(t *testing.T) {
				testLoopInResume(
					t, loopdb.StateInitiated, true,
					version, scriptVersion,
				)
			})

			t.Run("htlc published", func(t *testing.T) {
				testLoopInResume(
					t, loopdb.StateHtlcPublished, false,
					version, scriptVersion,
				)
			})
		})
	}
}

func testLoopInResume(t *testing.T, state loopdb.SwapState, expired bool,
	storedVersion loopdb.ProtocolVersion, scriptVersion swap.ScriptVersion) {

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
			ProtocolVersion: storedVersion,
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

	// If we have already published the htlc, we expect our cost to already
	// be published.
	var cost loopdb.SwapCost
	if state == loopdb.StateHtlcPublished {
		cost = loopdb.SwapCost{
			Onchain: 999,
		}
		pendSwap.Loop.Events[0].Cost = cost
	}

	htlc, err := swap.NewHtlc(
		scriptVersion, contract.CltvExpiry, contract.SenderKey,
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
		cost = loopdb.SwapCost{
			Onchain: getTxFee(
				&htlcTx, test.DefaultMockFee.FeePerKVByte(),
			),
		}

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
	ctx.assertSubscribeInvoice(testPreimage.Hash())

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	amtPaid := btcutil.Amount(49000)
	ctx.updateInvoiceState(amtPaid, channeldb.ContractSettled)

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
	finalState := ctx.store.assertLoopInState(loopdb.StateSuccess)

	// We expect our server fee to reflect as the difference between htlc
	// value and invoice amount paid. We use our original on-chain cost, set
	// earlier in the test, because we expect this value to be unchanged.
	cost.Server = btcutil.Amount(htlcTx.TxOut[0].Value) - amtPaid
	require.Equal(t, cost, finalState.Cost)
}
