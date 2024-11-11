package loop

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/routing/route"
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
	t.Run("stable protocol", func(t *testing.T) {
		testLoopInSuccess(t)
	})

	t.Run("experimental protocol", func(t *testing.T) {
		loopdb.EnableExperimentalProtocol()
		defer loopdb.ResetCurrentProtocolVersion()

		testLoopInSuccess(t)
	})
}

func testLoopInSuccess(t *testing.T) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg := newSwapConfig(&ctx.lnd.LndServices, ctx.store, ctx.server)

	expectedLastHop := &route.Vertex{0x02}

	req := &testLoopInRequest
	req.LastHop = expectedLastHop

	initResult, err := newLoopInSwap(
		context.Background(), cfg,
		height, req,
	)
	require.NoError(t, err)

	inSwap := initResult.swap

	ctx.store.AssertLoopInStored()

	errChan := make(chan error)
	go func() {
		err := inSwap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	swapInfo := <-ctx.statusChan
	require.Equal(t, loopdb.StateInitiated, swapInfo.State)

	// Check that the SwapInfo contains the provided last hop.
	require.Equal(t, expectedLastHop, swapInfo.LastHop)

	// Check that the SwapInfo does not contain an outgoing chan set.
	require.Nil(t, swapInfo.OutgoingChanSet)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)

	// Expect htlc to be published.
	htlcTx := <-ctx.lnd.SendOutputsChannel

	// We expect our cost to use the mock fee rate we set for our conf
	// target.
	cost := loopdb.SwapCost{
		Onchain: getTxFee(&htlcTx, test.DefaultMockFee.FeePerKVByte()),
	}

	// Expect the same state to be written again with the htlc tx hash
	// and on chain fee.
	state := ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)
	require.NotNil(t, state.HtlcTxHash)
	require.Equal(t, cost, state.Cost)

	// Expect register for htlc conf (only one, since the htlc is p2tr).
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
	ctx.updateInvoiceState(49000, invpkg.ContractSettled)

	// Swap is expected to move to the state InvoiceSettled
	ctx.assertState(loopdb.StateInvoiceSettled)
	ctx.store.AssertLoopInState(loopdb.StateInvoiceSettled)

	// Server spends htlc.
	successTx := wire.MsgTx{}
	witness, err := inSwap.htlc.GenSuccessWitness(
		[]byte{}, inSwap.contract.Preimage,
	)
	require.NoError(t, err)

	successTx.AddTxIn(&wire.TxIn{
		Witness: witness,
	})

	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        &successTx,
		SpenderInputIndex: 0,
	}

	ctx.assertState(loopdb.StateSuccess)
	ctx.store.AssertLoopInState(loopdb.StateSuccess)

	require.NoError(t, <-errChan)
}

// TestLoopInTimeout tests scenarios where the server doesn't sweep the htlc
// and the client is forced to reclaim the funds using the timeout tx.
func TestLoopInTimeout(t *testing.T) {
	testAmt := int64(testLoopInRequest.Amount)
	testCases := []struct {
		name          string
		externalValue int64
	}{
		{
			name:          "internal htlc",
			externalValue: 0,
		},
		{
			name:          "external htlc",
			externalValue: testAmt,
		},
		{
			name:          "external htlc amount too high",
			externalValue: testAmt + 1,
		},
		{
			name:          "external htlc amount too low",
			externalValue: testAmt - 1,
		},
	}

	for _, next := range []bool{false, true} {
		next := next

		for _, testCase := range testCases {
			testCase := testCase

			name := testCase.name
			if next {
				name += " experimental protocol"
			}

			t.Run(name, func(t *testing.T) {
				if next {
					loopdb.EnableExperimentalProtocol()
					defer loopdb.ResetCurrentProtocolVersion()
				}

				testLoopInTimeout(t, testCase.externalValue)
			})
		}
	}
}

func testLoopInTimeout(t *testing.T, externalValue int64) {
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
	require.NoError(t, err)
	inSwap := initResult.swap

	ctx.store.AssertLoopInStored()

	errChan := make(chan error)
	go func() {
		err := inSwap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	ctx.assertState(loopdb.StateInitiated)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)

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
		state := ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)
		require.NotNil(t, state.HtlcTxHash)
		require.Equal(t, cost, state.Cost)
	} else {
		// Create an external htlc publish tx.
		var pkScript []byte
		if !IsTaprootSwap(&inSwap.SwapContract) {
			pkScript = inSwap.htlcP2WSH.PkScript
		} else {
			pkScript = inSwap.htlcP2TR.PkScript
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

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	isInvalidAmt := externalValue != 0 && externalValue != int64(req.Amount)
	handleHtlcExpiry(
		t, ctx, inSwap, htlcTx, cost, errChan, isInvalidAmt,
	)
}

func handleHtlcExpiry(t *testing.T, ctx *loopInTestContext, inSwap *loopInSwap,
	htlcTx wire.MsgTx, cost loopdb.SwapCost, errChan chan error,
	isInvalidAmount bool) {

	if isInvalidAmount {
		ctx.store.AssertLoopInState(loopdb.StateFailIncorrectHtlcAmt)
		ctx.assertState(loopdb.StateFailIncorrectHtlcAmt)
	}

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel

	// Client starts listening for swap invoice updates.
	ctx.assertSubscribeInvoice(ctx.server.swapHash)

	// Let htlc expire.
	ctx.blockEpochChan <- inSwap.LoopInContract.CltvExpiry

	// Expect a signing request for the htlc tx output value.
	signReq := <-ctx.lnd.SignOutputRawChannel
	require.Equal(
		t, htlcTx.TxOut[0].Value,
		signReq.SignDescriptors[0].Output.Value,
		"invalid signing amount",
	)

	// Expect timeout tx to be published.
	timeoutTx := <-ctx.lnd.TxPublishChannel

	label := fmt.Sprintf("loopin-timeout-%x", inSwap.hash[:6])

	// We can just get our sweep fee as we would in the swap code because
	// our estimate is static.
	fee, err := inSwap.sweeper.GetSweepFee(
		context.Background(), inSwap.htlc.AddTimeoutToEstimator,
		inSwap.timeoutAddr, TimeoutTxConfTarget, label,
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
	ctx.updateInvoiceState(0, invpkg.ContractCanceled)

	var state loopdb.SwapStateData
	if isInvalidAmount {
		state = ctx.store.AssertLoopInState(
			loopdb.StateFailIncorrectHtlcAmtSwept,
		)
		ctx.assertState(loopdb.StateFailIncorrectHtlcAmtSwept)
	} else {
		ctx.assertState(loopdb.StateFailTimeout)
		state = ctx.store.AssertLoopInState(loopdb.StateFailTimeout)
	}
	require.Equal(t, cost, state.Cost)

	require.NoError(t, <-errChan)
}

// TestLoopInResume tests resuming swaps in various states.
func TestLoopInResume(t *testing.T) {
	storedVersion := []loopdb.ProtocolVersion{
		loopdb.ProtocolVersionUnrecorded,
		loopdb.ProtocolVersionHtlcV2,
		loopdb.ProtocolVersionHtlcV3,
		loopdb.ProtocolVersionMuSig2,
	}

	testCases := []struct {
		name    string
		state   loopdb.SwapState
		expired bool
	}{
		{
			name:    "initiated",
			state:   loopdb.StateInitiated,
			expired: false,
		},
		{
			name:    "initiated expired",
			state:   loopdb.StateInitiated,
			expired: true,
		},
		{
			name:    "htlc published",
			state:   loopdb.StateHtlcPublished,
			expired: false,
		},
	}

	for _, next := range []bool{false, true} {
		for _, version := range storedVersion {
			version := version
			for _, testCase := range testCases {
				testCase := testCase

				name := fmt.Sprintf(
					"%v %v", testCase, version.String(),
				)
				if next {
					name += " next protocol"
				}

				t.Run(name, func(t *testing.T) {
					testLoopInResume(
						t, testCase.state,
						testCase.expired,
						version,
					)
				})
			}
		}
	}
}

func testLoopInResume(t *testing.T, state loopdb.SwapState, expired bool,
	storedVersion loopdb.ProtocolVersion) {

	defer test.Guard(t)()
	ctxb := context.Background()

	ctx := newLoopInTestContext(t)
	cfg := newSwapConfig(&ctx.lnd.LndServices, ctx.store, ctx.server)

	// Create sender and receiver keys.
	_, senderPubKey := test.CreateKey(1)
	_, receiverPubKey := test.CreateKey(2)

	var senderKey, receiverKey [33]byte
	copy(receiverKey[:], receiverPubKey.SerializeCompressed())
	copy(senderKey[:], senderPubKey.SerializeCompressed())

	contract := &loopdb.LoopInContract{
		HtlcConfTarget: 2,
		SwapContract: loopdb.SwapContract{
			Preimage:        testPreimage,
			AmountRequested: 100000,
			CltvExpiry:      744,
			HtlcKeys: loopdb.HtlcKeys{
				SenderScriptKey:        senderKey,
				SenderInternalPubKey:   senderKey,
				ReceiverScriptKey:      receiverKey,
				ReceiverInternalPubKey: receiverKey,
			},
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

	htlc, err := utils.GetHtlc(
		testPreimage.Hash(), &contract.SwapContract,
		cfg.lnd.ChainParams,
	)
	require.NoError(t, err)

	err = ctx.store.CreateLoopIn(ctxb, testPreimage.Hash(), contract)
	require.NoError(t, err)

	inSwap, err := resumeLoopInSwap(context.Background(), cfg, pendSwap)
	require.NoError(t, err)

	var height int32
	if expired {
		height = 740
	} else {
		height = 600
	}

	errChan := make(chan error)
	go func() {
		err := inSwap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		errChan <- err
	}()

	defer func() {
		require.NoError(t, <-errChan)

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
		ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)

		// Expect htlc to be published.
		htlcTx = <-ctx.lnd.SendOutputsChannel
		cost = loopdb.SwapCost{
			Onchain: getTxFee(
				&htlcTx, test.DefaultMockFee.FeePerKVByte(),
			),
		}

		// Expect the same state to be written again with the htlc tx
		// hash.
		state := ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)
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
	ctx.updateInvoiceState(amtPaid, invpkg.ContractSettled)

	// Swap is expected to move to the state InvoiceSettled
	ctx.assertState(loopdb.StateInvoiceSettled)
	ctx.store.AssertLoopInState(loopdb.StateInvoiceSettled)

	// Server spends htlc.
	successTx := wire.MsgTx{}
	witness, err := htlc.GenSuccessWitness([]byte{}, testPreimage)
	require.NoError(t, err)

	successTx.AddTxIn(&wire.TxIn{
		Witness: witness,
	})
	successTxHash := successTx.TxHash()

	ctx.lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        &successTx,
		SpenderTxHash:     &successTxHash,
		SpenderInputIndex: 0,
	}

	ctx.assertState(loopdb.StateSuccess)
	finalState := ctx.store.AssertLoopInState(loopdb.StateSuccess)

	// We expect our server fee to reflect as the difference between htlc
	// value and invoice amount paid. We use our original on-chain cost, set
	// earlier in the test, because we expect this value to be unchanged.
	cost.Server = btcutil.Amount(htlcTx.TxOut[0].Value) - amtPaid
	require.Equal(t, cost, finalState.Cost)
}

// TestAbandonPublishedHtlcState advances a loop-in swap to StateHtlcPublished,
// then abandons it and ensures that executing the same swap would not progress.
func TestAbandonPublishedHtlcState(t *testing.T) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg, err, inSwap := startNewLoopIn(t, ctx, height)
	require.NoError(t, err)

	advanceToPublishedHtlc(t, ctx)

	// The client requests to abandon the published htlc state.
	inSwap.abandonChan <- struct{}{}

	// Ensure that the swap is now in the StateFailAbandoned state.
	ctx.assertState(loopdb.StateFailAbandoned)

	// Ensure that the swap is also in the StateFailAbandoned state in the
	// database.
	ctx.store.AssertLoopInState(loopdb.StateFailAbandoned)

	// Ensure that the swap was abandoned and the execution stopped.
	err = <-ctx.errChan
	require.Error(t, err)
	require.Contains(t, err.Error(), "swap hash abandoned by client")

	// We re-instantiate the swap and ensure that it does not progress.
	pendSwap := &loopdb.LoopIn{
		Contract: &inSwap.LoopInContract,
		Loop: loopdb.Loop{
			Events: []*loopdb.LoopEvent{
				{
					SwapStateData: loopdb.SwapStateData{
						State: inSwap.state,
					},
				},
			},
			Hash: testPreimage.Hash(),
		},
	}
	resumedSwap, err := resumeLoopInSwap(
		context.Background(), cfg, pendSwap,
	)
	require.NoError(t, err)

	// Execute the abandoned swap.
	go func() {
		err := resumedSwap.execute(
			context.Background(), ctx.cfg, height,
		)
		if err != nil {
			log.Error(err)
		}
		ctx.errChan <- err
	}()

	// Ensure that the swap is still in the StateFailAbandoned state.
	swapInfo := <-ctx.statusChan
	require.Equal(t, loopdb.StateFailAbandoned, swapInfo.State)

	// Ensure that the execution flagged the abandoned swap as finalized.
	err = <-ctx.errChan
	require.Error(t, err)
	require.Equal(t, ErrSwapFinalized, err)
}

// TestAbandonSettledInvoiceState advances a loop-in swap to
// StateInvoiceSettled, then abandons it and ensures that executing the same
// swap would not progress.
func TestAbandonSettledInvoiceState(t *testing.T) {
	defer test.Guard(t)()

	ctx := newLoopInTestContext(t)

	height := int32(600)

	cfg, err, inSwap := startNewLoopIn(t, ctx, height)
	require.NoError(t, err)

	advanceToPublishedHtlc(t, ctx)

	// Client starts listening for swap invoice updates.
	ctx.assertSubscribeInvoice(ctx.server.swapHash)

	// Server has already paid invoice before spending the htlc. Signal
	// settled.
	ctx.updateInvoiceState(49000, invpkg.ContractSettled)

	// Swap is expected to move to the state InvoiceSettled
	ctx.assertState(loopdb.StateInvoiceSettled)
	ctx.store.AssertLoopInState(loopdb.StateInvoiceSettled)

	// The client requests to abandon the published htlc state.
	inSwap.abandonChan <- struct{}{}

	// Ensure that the swap is now in the StateFailAbandoned state.
	ctx.assertState(loopdb.StateFailAbandoned)

	// Ensure that the swap is also in the StateFailAbandoned state in the
	// database.
	ctx.store.AssertLoopInState(loopdb.StateFailAbandoned)

	// Ensure that the swap was abandoned and the execution stopped.
	err = <-ctx.errChan
	require.Error(t, err)
	require.Contains(t, err.Error(), "swap hash abandoned by client")

	// We re-instantiate the swap and ensure that it does not progress.
	pendSwap := &loopdb.LoopIn{
		Contract: &inSwap.LoopInContract,
		Loop: loopdb.Loop{
			Events: []*loopdb.LoopEvent{
				{
					SwapStateData: loopdb.SwapStateData{
						State: inSwap.state,
					},
				},
			},
			Hash: testPreimage.Hash(),
		},
	}
	resumedSwap, err := resumeLoopInSwap(context.Background(), cfg, pendSwap)
	require.NoError(t, err)

	// Execute the abandoned swap.
	go func() {
		err := resumedSwap.execute(
			context.Background(), ctx.cfg, height,
		)
		if err != nil {
			log.Error(err)
		}
		ctx.errChan <- err
	}()

	// Ensure that the swap is still in the StateFailAbandoned state.
	swapInfo := <-ctx.statusChan
	require.Equal(t, loopdb.StateFailAbandoned, swapInfo.State)

	// Ensure that the execution flagged the abandoned swap as finalized.
	err = <-ctx.errChan
	require.Error(t, err)
	require.Equal(t, ErrSwapFinalized, err)
}

func advanceToPublishedHtlc(t *testing.T, ctx *loopInTestContext) SwapInfo {
	swapInfo := <-ctx.statusChan
	require.Equal(t, loopdb.StateInitiated, swapInfo.State)

	ctx.assertState(loopdb.StateHtlcPublished)
	ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)

	// Expect htlc to be published.
	htlcTx := <-ctx.lnd.SendOutputsChannel

	// Expect the same state to be written again with the htlc tx hash
	// and on chain fee.
	ctx.store.AssertLoopInState(loopdb.StateHtlcPublished)

	// Expect register for htlc conf (only one, since the htlc is p2tr).
	<-ctx.lnd.RegisterConfChannel

	// Confirm htlc.
	ctx.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}

	// Client starts listening for spend of htlc.
	<-ctx.lnd.RegisterSpendChannel
	return swapInfo
}

func startNewLoopIn(t *testing.T, ctx *loopInTestContext, height int32) (
	*swapConfig, error, *loopInSwap) {

	cfg := newSwapConfig(&ctx.lnd.LndServices, ctx.store, ctx.server)

	req := &testLoopInRequest

	initResult, err := newLoopInSwap(
		context.Background(), cfg,
		height, req,
	)
	require.NoError(t, err)

	inSwap := initResult.swap

	ctx.store.AssertLoopInStored()

	go func() {
		err := inSwap.execute(context.Background(), ctx.cfg, height)
		if err != nil {
			log.Error(err)
		}
		ctx.errChan <- err
	}()
	return cfg, err, inSwap
}
