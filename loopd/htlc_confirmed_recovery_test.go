package loopd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	// htlcConfirmedRecoveryTestTimeout bounds recovery worker tests so
	// channel interactions cannot block the suite indefinitely.
	htlcConfirmedRecoveryTestTimeout = 5 * time.Second

	// htlcConfirmedRecoveryNoEventWait bounds negative channel assertions.
	htlcConfirmedRecoveryNoEventWait = 100 * time.Millisecond
)

// mockHtlcConfirmedSubscriber exposes a controllable HTLC-confirmed
// notification stream for recovery worker tests.
type mockHtlcConfirmedSubscriber struct {
	ntfnChan chan *swapserverrpc.ServerHtlcConfirmedNotification
}

// newMockHtlcConfirmedSubscriber creates a buffered notification source for
// the recovery worker tests.
func newMockHtlcConfirmedSubscriber() *mockHtlcConfirmedSubscriber {
	return &mockHtlcConfirmedSubscriber{
		ntfnChan: make(
			chan *swapserverrpc.ServerHtlcConfirmedNotification, 2,
		),
	}
}

// SubscribeHtlcConfirmed returns the test-controlled notification stream.
func (m *mockHtlcConfirmedSubscriber) SubscribeHtlcConfirmed(
	context.Context) <-chan *swapserverrpc.ServerHtlcConfirmedNotification {

	return m.ntfnChan
}

// htlcConfirmedRecoveryFixture bundles the mocks and swap data used by the
// recovery worker tests.
type htlcConfirmedRecoveryFixture struct {
	lnd          *test.LndMockServices
	store        *loopdb.StoreMock
	subscriber   *mockHtlcConfirmedSubscriber
	manager      *htlcConfirmedRecoveryManager
	swap         *loopdb.LoopOut
	htlc         *swap.Htlc
	fundingTx    *wire.MsgTx
	outpoint     wire.OutPoint
	destPkScript []byte

	// bestHeight and bestHeightErr are what the worker's height lookup
	// returns.
	bestHeight    int32
	bestHeightErr error
}

// newHtlcConfirmedRecoveryFixture builds a loop-out swap and the mock daemon
// services needed to recover its HTLC.
func newHtlcConfirmedRecoveryFixture(
	t *testing.T) *htlcConfirmedRecoveryFixture {

	t.Helper()

	lnd := test.NewMockLnd()
	store := loopdb.NewStoreMock(t)

	preimage := lntypes.Preimage{1, 2, 3, 4}
	swapHash := preimage.Hash()

	// Construct deterministic HTLC keys so the test reconstructs a stable
	// swap script and outpoint.
	_, senderPub := test.CreateKey(0)
	_, receiverPub := test.CreateKey(1)

	var senderKey, receiverKey [33]byte
	copy(senderKey[:], senderPub.SerializeCompressed())
	copy(receiverKey[:], receiverPub.SerializeCompressed())

	htlcKeys := loopdb.HtlcKeys{
		SenderScriptKey:   senderKey,
		ReceiverScriptKey: receiverKey,
		ClientScriptKeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.KeyFamily),
			Index:  7,
		},
	}

	destAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		bytes.Repeat([]byte{2}, 20), lnd.ChainParams,
	)
	require.NoError(t, err)

	swapContract := &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			Preimage:         preimage,
			AmountRequested:  1_000_000,
			HtlcKeys:         htlcKeys,
			CltvExpiry:       500,
			InitiationHeight: 123,
			ProtocolVersion:  loopdb.ProtocolVersionHtlcV2,
		},
		DestAddr: destAddr,
	}

	loopOut := &loopdb.LoopOut{
		Loop: loopdb.Loop{
			Hash: swapHash,
		},
		Contract: swapContract,
	}
	store.LoopOutSwaps[swapHash] = swapContract

	// Fund the reconstructed HTLC with a single output so the worker has a
	// recoverable success-path spend target.
	htlc, err := utils.GetHtlc(
		swapHash, &swapContract.SwapContract, lnd.ChainParams,
	)
	require.NoError(t, err)

	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    int64(swapContract.AmountRequested),
		PkScript: htlc.PkScript,
	})

	outpoint := wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: 0,
	}

	destPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	subscriber := newMockHtlcConfirmedSubscriber()
	fixture := &htlcConfirmedRecoveryFixture{
		lnd:          lnd,
		store:        store,
		subscriber:   subscriber,
		swap:         loopOut,
		htlc:         htlc,
		fundingTx:    fundingTx,
		outpoint:     outpoint,
		destPkScript: destPkScript,

		// Well before the last preimage reveal height of the swap.
		bestHeight: 322,
	}
	fixture.manager = &htlcConfirmedRecoveryManager{
		notificationSource: subscriber,
		swapStore:          store,
		chainParams:        lnd.ChainParams,
		notifier:           lnd.ChainNotifier,
		wallet:             lnd.WalletKit,
		signer:             lnd.Signer,
		bestHeight: func(context.Context) (int32, error) {
			return fixture.bestHeight, fixture.bestHeightErr
		},
	}

	return fixture
}

// waitForConfRegistration waits for the recovery worker to register its
// confirmation notification and returns the registration.
func waitForConfRegistration(t *testing.T, ctx context.Context,
	lnd *test.LndMockServices) *test.ConfRegistration {

	t.Helper()

	select {
	case reg := <-lnd.RegisterConfChannel:
		return reg

	case <-ctx.Done():
		t.Fatalf("timed out waiting for confirmation registration: %v",
			ctx.Err())

		return nil
	}
}

// waitForSignOutputRawRequest waits for the worker to request a signature.
func waitForSignOutputRawRequest(t *testing.T, ctx context.Context,
	lnd *test.LndMockServices) test.SignOutputRawRequest {

	t.Helper()

	select {
	case req := <-lnd.SignOutputRawChannel:
		return req

	case <-ctx.Done():
		t.Fatalf("timed out waiting for sign request: %v", ctx.Err())

		return test.SignOutputRawRequest{}
	}
}

// waitForPublishedTx waits for the recovery worker to publish its sweep.
func waitForPublishedTx(t *testing.T, ctx context.Context,
	lnd *test.LndMockServices) *wire.MsgTx {

	t.Helper()

	select {
	case tx := <-lnd.TxPublishChannel:
		return tx

	case <-ctx.Done():
		t.Fatalf("timed out waiting for published tx: %v", ctx.Err())

		return nil
	}
}

// waitForManagerExit waits for the recovery worker goroutine to stop.
func waitForManagerExit(t *testing.T, ctx context.Context,
	runErrChan <-chan error) error {

	t.Helper()

	select {
	case err := <-runErrChan:
		return err

	case <-ctx.Done():
		t.Fatalf("timed out waiting for recovery worker exit: %v",
			ctx.Err())

		return nil
	}
}

// assertNoRecoveryActivity verifies that the recovery worker did not start a
// sweep attempt.
func assertNoRecoveryActivity(t *testing.T, lnd *test.LndMockServices) {
	t.Helper()

	select {
	case reg := <-lnd.RegisterConfChannel:
		t.Fatalf("unexpected confirmation registration: %+v", reg)

	case <-time.After(htlcConfirmedRecoveryNoEventWait):
	}

	select {
	case req := <-lnd.SignOutputRawChannel:
		t.Fatalf("unexpected sign request: %+v", req)

	case <-time.After(htlcConfirmedRecoveryNoEventWait):
	}

	select {
	case tx := <-lnd.TxPublishChannel:
		t.Fatalf("unexpected published tx: %v", tx.TxHash())

	case <-time.After(htlcConfirmedRecoveryNoEventWait):
	}
}

// TestHtlcConfirmedRecoveryManagerPublishesSweep verifies that a valid
// notification triggers a direct sweep to the stored destination address.
func TestHtlcConfirmedRecoveryManagerPublishesSweep(t *testing.T) {
	defer test.Guard(t)()

	setLogger(newFormatLogger())

	fixture := newHtlcConfirmedRecoveryFixture(t)

	ctx, cancel := context.WithTimeout(
		t.Context(), htlcConfirmedRecoveryTestTimeout,
	)
	defer cancel()

	runErrChan := make(chan error, 1)

	go func() {
		runErrChan <- fixture.manager.run(ctx)
	}()

	ntfn := &swapserverrpc.ServerHtlcConfirmedNotification{
		SwapHash:     fixture.swap.Hash[:],
		HtlcOutpoint: fixture.outpoint.String(),
		HtlcAddress:  fixture.htlc.Address.String(),
		SatPerVbyte:  25,
	}
	fixture.subscriber.ntfnChan <- ntfn
	close(fixture.subscriber.ntfnChan)

	reg := waitForConfRegistration(t, ctx, fixture.lnd)
	require.NotNil(t, reg.TxID)
	require.Equal(t, fixture.outpoint.Hash, *reg.TxID)
	require.Equal(t, fixture.htlc.PkScript, reg.PkScript)

	fixture.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx:          fixture.fundingTx,
		BlockHeight: 321,
	}

	signReq := waitForSignOutputRawRequest(t, ctx, fixture.lnd)
	require.Equal(
		t, fixture.outpoint, signReq.Tx.TxIn[0].PreviousOutPoint,
	)
	require.Len(t, signReq.Tx.TxOut, 1)
	require.Equal(
		t, fixture.destPkScript, signReq.Tx.TxOut[0].PkScript,
	)

	publishedTx := waitForPublishedTx(t, ctx, fixture.lnd)
	require.Equal(
		t, fixture.outpoint, publishedTx.TxIn[0].PreviousOutPoint,
	)
	require.Len(t, publishedTx.TxOut, 1)
	require.Equal(
		t, fixture.destPkScript, publishedTx.TxOut[0].PkScript,
	)
	require.NotEmpty(t, publishedTx.TxIn[0].Witness)
	require.Equal(
		t, expectedHtlcConfirmedRecoveryFee(t, fixture, 25),
		htlcConfirmedRecoveryFee(
			fixture.swap.Contract.AmountRequested, publishedTx,
		),
	)

	require.NoError(t, waitForManagerExit(t, ctx, runErrChan))
	require.NoError(t, fixture.lnd.IsDone())
}

// TestHtlcConfirmedRecoveryManagerHonorsHtlcConfirmations verifies that the
// worker waits for the confirmation depth stored with the swap before it
// publishes the preimage, and for one confirmation when the stored depth is
// zero or one.
func TestHtlcConfirmedRecoveryManagerHonorsHtlcConfirmations(t *testing.T) {
	defer test.Guard(t)()

	setLogger(newFormatLogger())

	testCases := []struct {
		name        string
		storedConfs uint32
		expectConfs int32
	}{
		{
			name:        "unset depth uses one confirmation",
			storedConfs: 0,
			expectConfs: 1,
		},
		{
			name:        "depth one",
			storedConfs: 1,
			expectConfs: 1,
		},
		{
			name:        "depth two",
			storedConfs: 2,
			expectConfs: 2,
		},
		{
			name:        "depth six",
			storedConfs: 6,
			expectConfs: 6,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newHtlcConfirmedRecoveryFixture(t)
			fixture.swap.Contract.HtlcConfirmations =
				testCase.storedConfs

			ctx, cancel := context.WithTimeout(
				t.Context(), htlcConfirmedRecoveryTestTimeout,
			)
			defer cancel()

			runErrChan := make(chan error, 1)

			go func() {
				runErrChan <- fixture.manager.run(ctx)
			}()

			ntfn := &swapserverrpc.ServerHtlcConfirmedNotification{
				SwapHash:     fixture.swap.Hash[:],
				HtlcOutpoint: fixture.outpoint.String(),
				HtlcAddress:  fixture.htlc.Address.String(),
				SatPerVbyte:  25,
			}
			fixture.subscriber.ntfnChan <- ntfn
			close(fixture.subscriber.ntfnChan)

			reg := waitForConfRegistration(t, ctx, fixture.lnd)
			require.Equal(t, testCase.expectConfs, reg.NumConfs)

			fixture.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
				Tx:          fixture.fundingTx,
				BlockHeight: 321,
			}

			waitForSignOutputRawRequest(t, ctx, fixture.lnd)
			waitForPublishedTx(t, ctx, fixture.lnd)

			require.NoError(
				t, waitForManagerExit(t, ctx, runErrChan),
			)
			require.NoError(t, fixture.lnd.IsDone())
		})
	}
}

// TestHtlcConfirmedRecoveryManagerIgnoresZeroFeeRate verifies that a zero-fee
// notification is rejected by the shared sweep path without publishing a tx.
func TestHtlcConfirmedRecoveryManagerIgnoresZeroFeeRate(t *testing.T) {
	defer test.Guard(t)()

	setLogger(newFormatLogger())

	fixture := newHtlcConfirmedRecoveryFixture(t)

	ctx, cancel := context.WithTimeout(
		t.Context(), htlcConfirmedRecoveryTestTimeout,
	)
	defer cancel()

	runErrChan := make(chan error, 1)

	go func() {
		runErrChan <- fixture.manager.run(ctx)
	}()

	ntfn := &swapserverrpc.ServerHtlcConfirmedNotification{
		SwapHash:     fixture.swap.Hash[:],
		HtlcOutpoint: fixture.outpoint.String(),
		HtlcAddress:  fixture.htlc.Address.String(),
	}
	fixture.subscriber.ntfnChan <- ntfn
	close(fixture.subscriber.ntfnChan)

	assertNoRecoveryActivity(t, fixture.lnd)

	require.NoError(t, waitForManagerExit(t, ctx, runErrChan))
	require.NoError(t, fixture.lnd.IsDone())
}

// TestHtlcConfirmedRecoveryManagerIgnoresBadNotification verifies that a bad
// notification is ignored without starting a sweep attempt.
func TestHtlcConfirmedRecoveryManagerIgnoresBadNotification(t *testing.T) {
	defer test.Guard(t)()

	setLogger(newFormatLogger())

	fixture := newHtlcConfirmedRecoveryFixture(t)

	ctx, cancel := context.WithTimeout(
		t.Context(), htlcConfirmedRecoveryTestTimeout,
	)
	defer cancel()

	runErrChan := make(chan error, 1)

	go func() {
		runErrChan <- fixture.manager.run(ctx)
	}()

	ntfn := &swapserverrpc.ServerHtlcConfirmedNotification{
		SwapHash:     fixture.swap.Hash[:],
		HtlcOutpoint: "not-an-outpoint",
		HtlcAddress:  fixture.htlc.Address.String(),
	}
	fixture.subscriber.ntfnChan <- ntfn
	close(fixture.subscriber.ntfnChan)

	require.NoError(t, waitForManagerExit(t, ctx, runErrChan))
	assertNoRecoveryActivity(t, fixture.lnd)
	require.NoError(t, fixture.lnd.IsDone())
}

// htlcConfirmedRecoveryFee returns the fee paid by a single-input recovery
// sweep transaction.
func htlcConfirmedRecoveryFee(inputValue btcutil.Amount,
	tx *wire.MsgTx) btcutil.Amount {

	outputValue := btcutil.Amount(tx.TxOut[0].Value)

	return inputValue - outputValue
}

// expectedHtlcConfirmedRecoveryFee computes the fee using the same estimator
// inputs as sweepHtlc.
func expectedHtlcConfirmedRecoveryFee(t *testing.T,
	fixture *htlcConfirmedRecoveryFixture, satPerVByte uint32) btcutil.Amount {

	t.Helper()

	var estimator input.TxWeightEstimator
	err := fixture.htlc.AddSuccessToEstimator(&estimator)
	require.NoError(t, err)

	err = sweep.AddOutputEstimate(&estimator, fixture.swap.Contract.DestAddr)
	require.NoError(t, err)

	feeRate := chainfee.SatPerVByte(satPerVByte).FeePerKWeight()

	return feeRate.FeeForWeightRoundUp(estimator.Weight())
}

// TestHtlcConfirmedRecoveryManagerRevealDeadline verifies that the worker
// doesn't reveal the preimage once too few blocks are left before the HTLC
// expires, unless the swap already revealed it.
func TestHtlcConfirmedRecoveryManagerRevealDeadline(t *testing.T) {
	defer test.Guard(t)()

	setLogger(newFormatLogger())

	// The fixture's HTLC expires at height 500.
	const revealDeadline = 500 - loop.MinLoopOutPreimageRevealDelta

	testCases := []struct {
		name          string
		height        int32
		heightErr     error
		state         loopdb.SwapState
		noHeightCheck bool
		expectSweep   bool
	}{
		{
			name:        "one block before the deadline",
			height:      revealDeadline - 1,
			expectSweep: true,
		},
		{
			name:   "at the deadline",
			height: revealDeadline,
		},
		{
			name:   "after the htlc expiry",
			height: 501,
		},
		{
			name:        "at the deadline but already revealed",
			height:      revealDeadline,
			state:       loopdb.StatePreimageRevealed,
			expectSweep: true,
		},
		{
			name:   "at the deadline with a timed out swap",
			height: revealDeadline,
			state:  loopdb.StateFailTimeout,
		},
		{
			name:      "height lookup fails",
			height:    322,
			heightErr: errors.New("lnd unavailable"),
		},
		{
			name:          "no height lookup",
			height:        322,
			noHeightCheck: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newHtlcConfirmedRecoveryFixture(t)
			fixture.bestHeight = testCase.height
			fixture.bestHeightErr = testCase.heightErr
			if testCase.state != loopdb.StateInitiated {
				state := loopdb.SwapStateData{
					State: testCase.state,
				}
				fixture.store.LoopOutUpdates[fixture.swap.Hash] =
					[]loopdb.SwapStateData{state}
			}
			if testCase.noHeightCheck {
				fixture.manager.bestHeight = nil
			}

			ctx, cancel := context.WithTimeout(
				t.Context(), htlcConfirmedRecoveryTestTimeout,
			)
			defer cancel()

			runErrChan := make(chan error, 1)

			go func() {
				runErrChan <- fixture.manager.run(ctx)
			}()

			ntfn := &swapserverrpc.ServerHtlcConfirmedNotification{
				SwapHash:     fixture.swap.Hash[:],
				HtlcOutpoint: fixture.outpoint.String(),
				HtlcAddress:  fixture.htlc.Address.String(),
				SatPerVbyte:  25,
			}
			fixture.subscriber.ntfnChan <- ntfn
			close(fixture.subscriber.ntfnChan)

			// Without a height lookup the worker can't check the
			// deadline, so it must not start a sweep at all.
			if testCase.noHeightCheck {
				err := waitForManagerExit(t, ctx, runErrChan)
				require.NoError(t, err)
				assertNoRecoveryActivity(t, fixture.lnd)

				return
			}

			waitForConfRegistration(t, ctx, fixture.lnd)
			fixture.lnd.ConfChannel <- &chainntnfs.TxConfirmation{
				Tx:          fixture.fundingTx,
				BlockHeight: 321,
			}

			if testCase.expectSweep {
				waitForSignOutputRawRequest(t, ctx, fixture.lnd)
				publishedTx := waitForPublishedTx(
					t, ctx, fixture.lnd,
				)
				require.Equal(
					t, fixture.outpoint,
					publishedTx.TxIn[0].PreviousOutPoint,
				)
			}

			// The worker is done with the notification once the
			// manager exits. A refused sweep must not have signed
			// or published anything by then.
			err := waitForManagerExit(t, ctx, runErrChan)
			require.NoError(t, err)
			if !testCase.expectSweep {
				assertNoRecoveryActivity(t, fixture.lnd)
			}
			require.NoError(t, fixture.lnd.IsDone())
		})
	}
}
