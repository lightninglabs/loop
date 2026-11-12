package loopin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const testTimeout = 5 * time.Second

// testSwapInvoice stands in for the swap invoice of a loop-in that added one.
const testSwapInvoice = "lnbcrt1swapinvoice"

// TestHandleInvoiceUpdate verifies that invoice state updates map to the
// monitor events expected by the static address loop-in FSM.
func TestHandleInvoiceUpdate(t *testing.T) {
	t.Parallel()

	swapHash := lntypes.Hash{1, 2, 3}
	tests := []struct {
		name      string
		state     invoices.ContractState
		event     fsm.EventType
		done      bool
		errString string
	}{
		{
			name:  "open",
			state: invoices.ContractOpen,
			event: fsm.NoOp,
		},
		{
			name:  "accepted",
			state: invoices.ContractAccepted,
			event: fsm.NoOp,
		},
		{
			name:  "settled",
			state: invoices.ContractSettled,
			event: OnPaymentReceived,
			done:  true,
		},
		{
			name:  "canceled",
			state: invoices.ContractCanceled,
			event: fsm.NoOp,
		},
		{
			name:  "unexpected",
			state: invoices.ContractState(99),
			event: fsm.NoOp,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := &FSM{
				StateMachine: &fsm.StateMachine{},
				loopIn: &StaticAddressLoopIn{
					SwapHash: swapHash,
				},
			}

			event, done := f.handleInvoiceUpdate(
				lndclient.InvoiceUpdate{
					Invoice: lndclient.Invoice{
						State: test.state,
					},
				},
			)
			require.Equal(t, test.event, event)
			require.Equal(t, test.done, done)

			if test.errString == "" {
				require.Nil(t, f.LastActionError)
			} else {
				require.ErrorContains(
					t, f.LastActionError, test.errString,
				)
				require.ErrorContains(
					t, f.LastActionError, fmt.Sprint(swapHash),
				)
			}
		})
	}
}

// TestMonitorInvoiceSettledWinsOverRecoveredRiskRejection verifies that an
// authoritative settled state takes precedence over a persisted server risk
// rejection during recovery.
func TestMonitorInvoiceSettledWinsOverRecoveredRiskRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{1, 2, 5}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractSettled,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionRejected,
		mockLnd.LndServices.Invoices,
	)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case event := <-resultChan:
		require.Equal(t, OnPaymentReceived, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Empty(t, depositMgr.transitions)
	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("settled invoice was canceled: %v", hash)

	default:
	}
}

// TestMonitorInvoiceCancelErrorKeepsMonitoring verifies that cancellation
// failures neither unlock deposits nor stop invoice monitoring. Recovery
// rechecks the authoritative invoice state, and a later settlement wins.
func TestMonitorInvoiceCancelErrorKeepsMonitoring(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{1, 2, 6}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	cancelCalls := make(chan lntypes.Hash, 2)
	releaseCancel := make(chan struct{})
	invoicesClient := &failingCancelInvoices{
		InvoicesClient: mockLnd.LndServices.Invoices,
		cancelCalls:    cancelCalls,
		release:        releaseCancel,
		err:            errors.New("invoice backend unavailable"),
	}
	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionRejected,
		invoicesClient,
	)
	f.ActionEntryFunc = nil

	resultChan := make(chan error, 1)
	go func() {
		resultChan <- f.SendEvent(ctx, OnRecover, nil)
	}()
	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-cancelCalls:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("cancellation attempt not received: %v", ctx.Err())
	}

	select {
	case transition := <-depositMgr.transitionChan:
		t.Fatalf("deposits unlocked after cancellation error: %v",
			transition)

	default:
	}

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractSettled,
	})
	close(releaseCancel)

	select {
	case err := <-resultChan:
		require.NoError(t, err)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Equal(t, []fsm.StateType{deposit.LoopedIn}, depositMgr.states)
}

// TestMonitorInvoiceUnknownStateKeepsMonitoring verifies that a failed lookup
// and an unknown subscription state do not release deposits. A subsequent
// authoritative settlement still completes the swap.
func TestMonitorInvoiceUnknownStateKeepsMonitoring(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{1, 2, 7}
	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	var invoiceSub *test.SingleInvoiceSubscription
	select {
	case invoiceSub = <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	select {
	case <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	invoiceSub.Update <- lndclient.InvoiceUpdate{
		Invoice: lndclient.Invoice{
			Hash:  swapHash,
			State: invoices.ContractState(99),
		},
	}

	select {
	case event := <-resultChan:
		t.Fatalf("unknown invoice state ended monitor with %v", event)

	case transition := <-depositMgr.transitionChan:
		t.Fatalf("unknown invoice state unlocked deposits: %v", transition)

	case <-time.After(100 * time.Millisecond):
	}

	invoiceSub.Update <- lndclient.InvoiceUpdate{
		Invoice: lndclient.Invoice{
			Hash:  swapHash,
			State: invoices.ContractSettled,
		},
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnPaymentReceived, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Empty(t, depositMgr.transitions)
}

// TestMonitorInvoiceSetupFailureRecoversSettledInvoice verifies that a
// transient subscription failure cannot route an already-settled swap through
// deposit cleanup before the authoritative invoice lookup runs.
func TestMonitorInvoiceSetupFailureRecoversSettledInvoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{1, 2, 8}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractSettled,
	})
	invoicesClient := &flakySubscribeInvoices{
		InvoicesClient: mockLnd.LndServices.Invoices,
		err:            errors.New("invoice backend unavailable"),
	}
	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionRejected,
		invoicesClient,
	)
	f.ActionEntryFunc = nil

	resultChan := make(chan error, 1)
	go func() {
		resultChan <- f.SendEvent(ctx, OnRecover, nil)
	}()

	select {
	case err := <-resultChan:
		require.NoError(t, err)

	case <-ctx.Done():
		t.Fatalf("monitor did not recover: %v", ctx.Err())
	}

	require.Equal(t, 1, invoicesClient.subscribeCalls)
	require.Equal(t, []fsm.StateType{deposit.LoopedIn}, depositMgr.states)
	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("settled invoice was canceled: %v", hash)

	default:
	}
}

// TestMonitorInvoiceStreamErrorRecoversSettlement verifies that a dead invoice
// stream checks the latest invoice state before entering recovery.
func TestMonitorInvoiceStreamErrorRecoversSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{1, 2, 9}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})
	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	f.cfg.LndClient = &firstLookupBarrier{
		LightningClient: mockLnd.Client,
		lookupStarted:   lookupStarted,
		release:         releaseLookup,
	}
	f.ActionEntryFunc = nil

	resultChan := make(chan error, 1)
	go func() {
		resultChan <- f.SendEvent(ctx, OnRecover, nil)
	}()

	var invoiceSub *test.SingleInvoiceSubscription
	select {
	case invoiceSub = <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}
	select {
	case <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}
	select {
	case <-lookupStarted:
	case <-ctx.Done():
		t.Fatalf("initial invoice lookup not received: %v", ctx.Err())
	}

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractSettled,
	})
	close(releaseLookup)
	select {
	case invoiceSub.Err <- errors.New("invoice stream failed"):
	case <-ctx.Done():
		t.Fatalf("invoice stream error was not consumed: %v", ctx.Err())
	}

	select {
	case err := <-resultChan:
		require.NoError(t, err)

	case <-ctx.Done():
		t.Fatalf("monitor did not recover: %v", ctx.Err())
	}

	require.Equal(t, []fsm.StateType{deposit.LoopedIn}, depositMgr.states)
}

// TestMonitorInvoiceAndHtlcTxReRegistersOnConfErr ensures that an error from
// the HTLC confirmation subscription triggers a re-registration. Without the
// regression fix, only the initial registration would be performed and the
// test would time out waiting for the second one.
func TestMonitorInvoiceAndHtlcTxReRegistersOnConfErr(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{1, 2, 3}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now().Add(-time.Hour),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	// Seed the mock invoice store so LookupInvoice succeeds.
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: &noopDepositManager{},
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	// Capture the invoice subscription the action registers so we can feed
	// an update later and let the action exit.
	var invSub *test.SingleInvoiceSubscription
	select {
	case invSub = <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	// The first confirmation registration should happen immediately.
	var firstReg *test.ConfRegistration
	select {
	case firstReg = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	// Force the confirmation stream to error so the FSM re-registers.
	firstReg.ErrChan <- errors.New("test htlc conf error")

	// FSM registers again, otherwise it would time out.
	var secondReg *test.ConfRegistration
	select {
	case secondReg = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf was not re-registered: %v", ctx.Err())
	}

	require.NotEqual(t, firstReg, secondReg)

	// Settle the invoice to let the action exit.
	invSub.Update <- lndclient.InvoiceUpdate{
		Invoice: lndclient.Invoice{
			Hash:  swapHash,
			State: invoices.ContractSettled,
		},
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnPaymentReceived, event)
	case <-ctx.Done():
		t.Fatalf("fsm did not return: %v", ctx.Err())
	}
}

// TestMonitorInvoiceAndHtlcTxNoOpOnShutdown ensures that a shutdown while the
// client is monitoring an HTLC-signed loop-in keeps the swap resumable instead
// of entering the generic unlock path.
func TestMonitorInvoiceAndHtlcTxNoOpOnShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	runCtx, stop := context.WithCancel(ctx)

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{4, 5, 6}
	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.Invoices[swapHash] = &lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	}

	depositMgr := &recordingDepositManager{}
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(runCtx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(runCtx, nil)
	}()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	select {
	case <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	stop()

	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Nil(t, f.LastActionError)
	require.Empty(t, depositMgr.transitions)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled on shutdown: %v", hash)

	default:
	}
}

// TestSweepHtlcTimeoutActionNoOpOnShutdown ensures that a shutdown during
// timeout sweep publication keeps the FSM in the same state so it can resume
// after restart.
func TestSweepHtlcTimeoutActionNoOpOnShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	mockLnd := test.NewMockLnd()
	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			LndClient: mockLnd.Client,
			WalletKit: mockLnd.WalletKit,
		},
		loopIn: &StaticAddressLoopIn{},
	}

	event := f.SweepHtlcTimeoutAction(ctx, nil)
	require.Equal(t, fsm.NoOp, event)
	require.Nil(t, f.LastActionError)
}

// TestMonitorHtlcTimeoutSweepActionNoOpOnShutdown ensures that a shutdown
// while waiting for the timeout sweep confirmation keeps the FSM resumable.
func TestMonitorHtlcTimeoutSweepActionNoOpOnShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	sweepAddr, err := mockLnd.WalletKit.NextAddr(ctx, "", 0, false)
	require.NoError(t, err)

	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			ChainNotifier: mockLnd.ChainNotifier,
		},
		loopIn: &StaticAddressLoopIn{
			HtlcTimeoutSweepAddress: sweepAddr,
			InitiationHeight:        uint32(mockLnd.Height),
		},
	}

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorHtlcTimeoutSweepAction(ctx, nil)
	}()

	select {
	case <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("timeout sweep conf registration not received: %v",
			ctx.Err())
	}

	cancel()

	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)
		require.Nil(t, f.LastActionError)

	case <-time.After(testTimeout):
		t.Fatal("timeout sweep monitor did not return")
	}
}

// TestMonitorInvoiceAndHtlcTxShutdownDoesNotUnlock verifies that daemon
// shutdown exits the monitor action without treating the swap as failed.
func TestMonitorInvoiceAndHtlcTxShutdownDoesNotUnlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	runCtx, stop := context.WithCancel(ctx)

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{1, 2, 4}
	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now().Add(-time.Hour),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		Deposits: []*deposit.Deposit{{
			Value: 200_000,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	depositMgr.transitionChan = make(chan depositTransition, 1)
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(runCtx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(runCtx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	stop()

	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.NoError(t, f.LastActionError)

	select {
	case transition := <-depositMgr.transitionChan:
		t.Fatalf("deposit transition on shutdown: %v", transition)

	default:
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled on shutdown: %v", hash)

	default:
	}
}

// TestInitHtlcActionPreservesRouteHints asserts that static-address loop-in
// propagates explicit route hints into the encoded swap invoice sent to the
// server.
func TestInitHtlcActionPreservesRouteHints(t *testing.T) {
	t.Parallel()

	mockLnd := test.NewMockLnd()
	_, serverKey := test.CreateKey(21)

	server := &mockStaticAddressServer{
		response: testStaticAddressLoopInResponse(
			serverKey.SerializeCompressed(),
		),
	}

	dep := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
		Value: 500_000,
	}

	loopIn := &StaticAddressLoopIn{
		Deposits:              []*deposit.Deposit{dep},
		DepositOutpoints:      []string{dep.OutPoint.String()},
		SelectedAmount:        dep.Value,
		QuotedSwapFee:         1_000,
		RouteHints:            testStaticAddressRouteHints(),
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		PaymentTimeoutSeconds: 3_600,
	}

	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			Server:                               server,
			DepositManager:                       &noopDepositManager{},
			LndClient:                            mockLnd.Client,
			WalletKit:                            mockLnd.WalletKit,
			ChainParams:                          mockLnd.ChainParams,
			Store:                                &mockStore{},
			ValidateLoopInContract:               testValidateLoopInContract,
			MaxStaticAddrHtlcFeePercentage:       1,
			MaxStaticAddrHtlcBackupFeePercentage: 1,
		},
		loopIn: loopIn,
	}

	event := f.InitHtlcAction(t.Context(), nil)
	require.Equal(t, OnHtlcInitiated, event)
	require.Nil(t, f.LastActionError)
	require.NotNil(t, server.request)

	_, routeHints, _, _, err := swap.DecodeInvoice(
		mockLnd.ChainParams, server.request.SwapInvoice,
	)
	require.NoError(t, err)

	test.RequireRouteHintsEqual(t, loopIn.RouteHints, routeHints)
}

func TestSignHtlcTxActionChecksDepositAvailability(t *testing.T) {
	dep := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x77},
			Index: 2,
		},
		Value: 200_000,
	}
	checker := &recordingTxOutChecker{}

	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			AddressManager: &mockAddressManager{
				params: &script.Parameters{
					ProtocolVersion: version.ProtocolVersion_V0,
				},
			},
			TxOutChecker: checker,
		},
		loopIn: &StaticAddressLoopIn{
			Deposits: []*deposit.Deposit{dep},
		},
	}

	event := f.SignHtlcTxAction(t.Context(), nil)
	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, f.LastActionError, "deposit "+
			dep.OutPoint.String()+" is no longer available",
	)
	require.Equal(t, [][]wire.OutPoint{{dep.OutPoint}}, checker.outpoints)
}

func TestCheckDepositsAvailableRejectsDivergentDepositOutpoints(
	t *testing.T) {

	currentOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x89},
		Index: 1,
	}
	snapshotOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x88},
		Index: 0,
	}
	checker := &recordingTxOutChecker{}

	f := &FSM{
		cfg: &Config{
			TxOutChecker: checker,
		},
		loopIn: &StaticAddressLoopIn{
			Deposits: []*deposit.Deposit{{
				OutPoint: currentOutpoint,
				Value:    200_000,
			}},
			DepositOutpoints: []string{snapshotOutpoint.String()},
		},
	}

	err := f.checkDepositsAvailable(t.Context())
	require.ErrorContains(t, err, "deposit outpoint snapshot mismatch")
	require.Empty(t, checker.outpoints)
}

// TestInitHtlcActionIgnoresSendUpdateErrorAfterPersistence protects the
// persistence-first invariant: once the loop-in is stored, a later status
// update failure must not roll back the action or state transition.
func TestInitHtlcActionIgnoresSendUpdateErrorAfterPersistence(t *testing.T) {
	mockLnd := test.NewMockLnd()
	_, serverKey := test.CreateKey(22)

	server := &mockStaticAddressServer{
		response: testStaticAddressLoopInResponse(
			serverKey.SerializeCompressed(),
		),
	}

	dep := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 0,
		},
		Value: 500_000,
	}

	loopIn := &StaticAddressLoopIn{
		Deposits:              []*deposit.Deposit{dep},
		DepositOutpoints:      []string{dep.OutPoint.String()},
		SelectedAmount:        dep.Value,
		QuotedSwapFee:         1_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		PaymentTimeoutSeconds: 3_600,
	}

	sendUpdateErr := errors.New("status channel blocked")
	sendUpdateCalled := false
	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			Server:                               server,
			DepositManager:                       &noopDepositManager{},
			LndClient:                            mockLnd.Client,
			WalletKit:                            mockLnd.WalletKit,
			ChainParams:                          mockLnd.ChainParams,
			Store:                                &mockStore{},
			ValidateLoopInContract:               testValidateLoopInContract,
			MaxStaticAddrHtlcFeePercentage:       1,
			MaxStaticAddrHtlcBackupFeePercentage: 1,
			SendUpdate: func(context.Context,
				*StaticAddressLoopIn) error {

				sendUpdateCalled = true

				return sendUpdateErr
			},
		},
		loopIn: loopIn,
	}

	event := f.InitHtlcAction(t.Context(), nil)
	require.Equal(t, OnHtlcInitiated, event)
	require.Nil(t, f.LastActionError)
	require.True(t, sendUpdateCalled)
}

// mockStaticAddressServer captures static-address loop-in requests in tests.
type mockStaticAddressServer struct {
	swapserverrpc.StaticAddressServerClient

	request  *swapserverrpc.ServerStaticAddressLoopInRequest
	response *swapserverrpc.ServerStaticAddressLoopInResponse
}

// ServerStaticAddressLoopIn records the request and returns the prepared
// response.
func (m *mockStaticAddressServer) ServerStaticAddressLoopIn(
	_ context.Context, in *swapserverrpc.ServerStaticAddressLoopInRequest,
	_ ...grpc.CallOption) (*swapserverrpc.ServerStaticAddressLoopInResponse,
	error) {

	m.request = in

	return m.response, nil
}

// testStaticAddressLoopInResponse returns a minimal successful server response
// for InitHtlcAction tests.
func testStaticAddressLoopInResponse(
	serverPubKey []byte) *swapserverrpc.ServerStaticAddressLoopInResponse {

	signingInfo := &swapserverrpc.ServerHtlcSigningInfo{
		FeeRate: 1,
	}

	return &swapserverrpc.ServerStaticAddressLoopInResponse{
		HtlcServerPubKey:   serverPubKey,
		HtlcExpiry:         1_000,
		StandardHtlcInfo:   signingInfo,
		HighFeeHtlcInfo:    signingInfo,
		ExtremeFeeHtlcInfo: signingInfo,
	}
}

// testStaticAddressRouteHints returns deterministic route hints for static
// loop-in invoice regression tests.
func testStaticAddressRouteHints() [][]zpay32.HopHint {
	_, pubKey1 := test.CreateKey(31)
	_, pubKey2 := test.CreateKey(32)
	_, pubKey3 := test.CreateKey(33)

	return [][]zpay32.HopHint{
		{
			{
				NodeID:                    pubKey1,
				ChannelID:                 11,
				FeeBaseMSat:               101,
				FeeProportionalMillionths: 201,
				CLTVExpiryDelta:           31,
			},
			{
				NodeID:                    pubKey2,
				ChannelID:                 12,
				FeeBaseMSat:               102,
				FeeProportionalMillionths: 202,
				CLTVExpiryDelta:           32,
			},
		},
		{
			{
				NodeID:                    pubKey3,
				ChannelID:                 13,
				FeeBaseMSat:               103,
				FeeProportionalMillionths: 203,
				CLTVExpiryDelta:           33,
			},
		},
	}
}

// testValidateLoopInContract accepts all server contract parameters in tests.
func testValidateLoopInContract(_ int32, _ int32) error {
	return nil
}

// TestMonitorInvoiceAndHtlcTxLocksConfirmedHtlcAtDeadline verifies that the
// payment timeout starts on risk acceptance and keeps confirmed HTLC deposits
// locked for timeout sweeping.
func TestMonitorInvoiceAndHtlcTxLocksConfirmedHtlcAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{4, 5, 6}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{7},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now().Add(-time.Hour),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 1,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	notificationMgr := &mockNotificationManager{
		riskAccepted: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification, 1,
		),
	}
	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	depositMgr.transitionChan = make(chan depositTransition, 1)

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      depositMgr,
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)
	htlcConf := htlcConfirmation(t, f)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	var confRegistration *test.ConfRegistration
	select {
	case confRegistration = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}
	confRegistration.ConfChan <- htlcConf

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled before risk acceptance: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	notificationMgr.riskAccepted <- &swapserverrpc.ServerStaticLoopInRiskAcceptedNotification{
		SwapHash: swapHash[:],
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled immediately after risk acceptance: %v",
			hash)

	case <-time.After(200 * time.Millisecond):
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, []*deposit.Deposit{
			loopIn.Deposits[0],
		}, transition.deposits)
		require.Equal(t, deposit.OnSweepingHtlcTimeout, transition.event)
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposits were not locked for timeout sweeping: %v",
			ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxIgnoresWrongHashRiskNotifications verifies that
// risk notifications for another swap do not start the payment deadline or
// persist a decision through the monitor action.
func TestMonitorInvoiceAndHtlcTxIgnoresWrongHashRiskNotifications(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{4, 5, 8}
	otherHash := lntypes.Hash{8, 5, 4}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	notificationMgr := &mockNotificationManager{
		riskAccepted: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification, 2,
		),
		riskRejected: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification, 1,
		),
	}
	store := &recordingRiskStore{
		mockStore: &mockStore{
			loopIns: map[lntypes.Hash]*StaticAddressLoopIn{
				swapHash: {},
			},
		},
		decisions: make(chan ConfirmationRiskDecision, 1),
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      &noopDepositManager{},
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
		Store:               store,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	notificationMgr.riskAccepted <- &swapserverrpc.
		ServerStaticLoopInRiskAcceptedNotification{
		SwapHash: otherHash[:],
	}
	notificationMgr.riskRejected <- &swapserverrpc.
		ServerStaticLoopInRiskRejectedNotification{
		SwapHash: otherHash[:],
	}

	select {
	case decision := <-store.decisions:
		t.Fatalf("persisted wrong-hash risk decision: %v", decision)

	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("canceled invoice for wrong-hash risk decision: %v", hash)

	case event := <-resultChan:
		t.Fatalf("monitor action exited after wrong-hash risk decision: %v",
			event)

	case <-time.After(200 * time.Millisecond):
	}

	notificationMgr.riskAccepted <- &swapserverrpc.
		ServerStaticLoopInRiskAcceptedNotification{
		SwapHash: swapHash[:],
	}

	select {
	case decision := <-store.decisions:
		require.Equal(t, ConfirmationRiskDecisionAccepted, decision)

	case <-ctx.Done():
		t.Fatalf("risk decision was not persisted: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxUsesPersistedAcceptedRiskTime verifies that live
// risk notifications use the durable receipt time, not the local channel
// receive time, when reconstructing the payment deadline.
func TestMonitorInvoiceAndHtlcTxUsesPersistedAcceptedRiskTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{4, 5, 7}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{8},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 1,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	notificationMgr := &mockNotificationManager{
		riskAccepted: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification, 1,
		),
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      &noopDepositManager{},
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
		Store: &mockStore{
			loopIns: map[lntypes.Hash]*StaticAddressLoopIn{
				swapHash: {
					ConfirmationRiskDecision: ConfirmationRiskDecisionAccepted,
					ConfirmationRiskDecisionTime: time.Now().Add(
						-time.Minute,
					),
				},
			},
		},
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled before risk acceptance: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	notificationMgr.riskAccepted <- &swapserverrpc.
		ServerStaticLoopInRiskAcceptedNotification{
		SwapHash: swapHash[:],
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxPersistsReplayedRiskAccepted verifies that a risk
// notification replayed after the swap row exists is written back to the store.
func TestMonitorInvoiceAndHtlcTxPersistsReplayedRiskAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{5, 6, 10}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{14},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	notificationMgr := &mockNotificationManager{
		riskAccepted: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification, 1,
		),
	}
	store := &recordingRiskStore{
		mockStore: &mockStore{
			loopIns: map[lntypes.Hash]*StaticAddressLoopIn{
				swapHash: {},
			},
		},
		decisions: make(chan ConfirmationRiskDecision, 1),
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      &noopDepositManager{},
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
		Store:               store,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	notificationMgr.riskAccepted <- &swapserverrpc.
		ServerStaticLoopInRiskAcceptedNotification{
		SwapHash: swapHash[:],
	}

	select {
	case decision := <-store.decisions:
		require.Equal(t, ConfirmationRiskDecisionAccepted, decision)

	case <-ctx.Done():
		t.Fatalf("risk decision was not persisted: %v", ctx.Err())
	}

	stored := store.loopIns[swapHash]
	require.Equal(t, ConfirmationRiskDecisionAccepted,
		stored.ConfirmationRiskDecision)
	require.False(t, stored.ConfirmationRiskDecisionTime.IsZero())

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxPersistsRiskRejected verifies that a server-side
// confirmation risk rejection is persisted and exits through the generic error
// path so the FSM unlocks deposits.
func TestMonitorInvoiceAndHtlcTxPersistsRiskRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{5, 6, 7}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        mockLnd.Height,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	notificationMgr := &mockNotificationManager{
		riskRejected: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification, 1,
		),
	}

	store := &recordingRiskStore{
		mockStore: &mockStore{
			loopIns: map[lntypes.Hash]*StaticAddressLoopIn{
				swapHash: {},
			},
		},
		decisions: make(chan ConfirmationRiskDecision, 1),
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      &noopDepositManager{},
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
		Store:               store,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	notificationMgr.riskRejected <- &swapserverrpc.ServerStaticLoopInRiskRejectedNotification{ // nolint: lll
		SwapHash: swapHash[:],
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case decision := <-store.decisions:
		require.Equal(t, ConfirmationRiskDecisionRejected, decision)

	case <-ctx.Done():
		t.Fatalf("risk decision was not persisted: %v", ctx.Err())
	}

	stored := store.loopIns[swapHash]
	require.Equal(t, ConfirmationRiskDecisionRejected,
		stored.ConfirmationRiskDecision)
	require.False(t, stored.ConfirmationRiskDecisionTime.IsZero())

	select {
	case event := <-resultChan:
		require.Equal(t, fsm.OnError, event)
		require.ErrorContains(
			t, f.LastActionError,
			"server rejected confirmation risk wait",
		)
	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxRecoversAcceptedRiskDecision verifies that a
// persisted risk acceptance restarts the payment deadline with elapsed time
// preserved after restart.
func TestMonitorInvoiceAndHtlcTxRecoversAcceptedRiskDecision(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{5, 6, 8}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{12},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:                     swapHash,
		SwapInvoice:                  testSwapInvoice,
		HtlcCltvExpiry:               2_000,
		InitiationHeight:             uint32(mockLnd.Height),
		InitiationTime:               time.Now(),
		ProtocolVersion:              version.ProtocolVersion_V0,
		ClientPubkey:                 clientKey.PubKey(),
		ServerPubkey:                 serverKey.PubKey(),
		PaymentTimeoutSeconds:        1,
		ConfirmationRiskDecision:     ConfirmationRiskDecisionAccepted,
		ConfirmationRiskDecisionTime: time.Now().Add(-time.Minute),
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	depositMgr.transitionChan = make(chan depositTransition, 1)

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, fsm.OnError, transition.event)
		require.Equal(t, deposit.Deposited, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposits were not unlocked: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxRecoversRejectedRiskDecision verifies that a
// persisted risk rejection still cancels after restart and exits through the
// generic error path so the FSM unlocks deposits.
func TestMonitorInvoiceAndHtlcTxRecoversRejectedRiskDecision(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{5, 6, 9}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{13},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:                     swapHash,
		SwapInvoice:                  testSwapInvoice,
		HtlcCltvExpiry:               mockLnd.Height,
		InitiationHeight:             uint32(mockLnd.Height),
		InitiationTime:               time.Now(),
		ProtocolVersion:              version.ProtocolVersion_V0,
		ClientPubkey:                 clientKey.PubKey(),
		ServerPubkey:                 serverKey.PubKey(),
		PaymentTimeoutSeconds:        3_600,
		ConfirmationRiskDecision:     ConfirmationRiskDecisionRejected,
		ConfirmationRiskDecisionTime: time.Now(),
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	depositMgr.transitionChan = make(chan depositTransition, 1)

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case event := <-resultChan:
		require.Equal(t, fsm.OnError, event)
		require.ErrorContains(
			t, f.LastActionError,
			"server rejected confirmation risk wait",
		)
	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxDoesNotCancelWhenOriginalOutpointVanishes
// verifies that once the monitor state is reached, a missing original deposit
// outpoint does not cancel the invoice. After HTLC signatures are handed to the
// server, the outpoint can disappear because the server published the expected
// HTLC transaction.
func TestMonitorInvoiceAndHtlcTxDoesNotCancelWhenOriginalOutpointVanishes(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{5, 7, 9}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{10},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	txOutChecker := &recordingTxOutChecker{}
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: &noopDepositManager{},
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
		TxOutChecker:   txOutChecker,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice should not have been canceled: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}

	require.Empty(t, txOutChecker.outpoints)
}

// TestMonitorInvoiceAndHtlcTxDoesNotCancelAcceptedInvoiceForMissingOutpoint
// verifies that the outpoint-vanished fallback is only active before payment
// has started. Once the invoice is accepted, the original deposit may disappear
// because the server has moved forward with the swap.
func TestMonitorInvoiceAndHtlcTxDoesNotCancelAcceptedInvoiceForMissingOutpoint(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{6, 8, 10}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{11},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractAccepted,
	})

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: &noopDepositManager{},
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
		TxOutChecker:   &recordingTxOutChecker{},
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice should not have been canceled: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case <-resultChan:

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxStartsDeadlineAtLegacyMinConfs verifies that the
// monitor action preserves the legacy payment deadline fallback when no risk
// decision has been observed locally.
func TestMonitorInvoiceAndHtlcTxStartsDeadlineAtLegacyMinConfs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{7, 8, 9}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{8},
		Index: 0,
	}
	depositRecord := &deposit.Deposit{
		OutPoint: depositOutpoint,
	}
	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 1,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{depositRecord},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier: mockLnd.ChainNotifier,
		DepositManager: &noopDepositManager{
			deposits: []*deposit.Deposit{depositRecord},
		},
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled before deposit confirmation: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	confirmationHeight := int64(mockLnd.Height) - deposit.MinConfs + 1
	depositRecord.Lock()
	depositRecord.ConfirmationHeight = confirmationHeight
	depositRecord.Unlock()

	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height))

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled immediately after deposit "+
			"confirmation: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxStartsLegacyFallbackWithNotificationManager
// verifies that the legacy payment deadline fallback still applies when the
// notification manager is configured but no risk decision has been observed.
func TestMonitorInvoiceAndHtlcTxStartsLegacyFallbackWithNotificationManager(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{7, 8, 10}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9},
		Index: 0,
	}
	depositRecord := &deposit.Deposit{
		OutPoint: depositOutpoint,
	}
	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now().Add(-time.Hour),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 1,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{depositRecord},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	notificationMgr := &mockNotificationManager{
		riskAccepted: make(
			chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification,
			1,
		),
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier: mockLnd.ChainNotifier,
		DepositManager: &noopDepositManager{
			deposits: []*deposit.Deposit{depositRecord},
		},
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	confirmationHeight := int64(mockLnd.Height) - deposit.MinConfs + 1
	depositRecord.Lock()
	depositRecord.ConfirmationHeight = confirmationHeight
	depositRecord.Unlock()

	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height))

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled before payment deadline: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxStartsLegacyFallbackAtCurrentHeight verifies that
// recovery can arm the legacy payment deadline without waiting for a later
// block notification.
func TestMonitorInvoiceAndHtlcTxStartsLegacyFallbackAtCurrentHeight(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{7, 8, 12}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{13},
		Index: 0,
	}
	staleDeposit := &deposit.Deposit{
		OutPoint:           depositOutpoint,
		ConfirmationHeight: 0,
	}
	confirmationHeight := int64(mockLnd.Height) - deposit.MinConfs + 1
	freshDeposit := &deposit.Deposit{
		OutPoint:           depositOutpoint,
		ConfirmationHeight: confirmationHeight,
	}
	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now().Add(-time.Hour),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 1,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{staleDeposit},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})
	store := &recordingRiskStore{
		mockStore: &mockStore{
			loopIns: map[lntypes.Hash]*StaticAddressLoopIn{
				swapHash: {},
			},
		},
		decisions: make(chan ConfirmationRiskDecision, 1),
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier: &silentBlockChainNotifier{
			ChainNotifierClient: mockLnd.ChainNotifier,
		},
		DepositManager: &noopDepositManager{
			deposits: []*deposit.Deposit{freshDeposit},
		},
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
		Store:          store,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case decision := <-store.decisions:
		require.Equal(t, ConfirmationRiskDecisionAccepted, decision)

	case <-ctx.Done():
		t.Fatalf("legacy fallback decision was not persisted: %v",
			ctx.Err())
	}
	require.Equal(t, ConfirmationRiskDecisionAccepted,
		store.loopIns[swapHash].ConfirmationRiskDecision)
	require.False(t,
		store.loopIns[swapHash].ConfirmationRiskDecisionTime.IsZero())

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled before payment deadline: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxRefreshesDepositsForLegacyFallback verifies that a
// recovered monitor state does not rely on stale selected-deposit snapshots when
// deciding whether the legacy payment deadline fallback has opened.
func TestMonitorInvoiceAndHtlcTxRefreshesDepositsForLegacyFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{7, 8, 11}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{12},
		Index: 0,
	}
	staleDeposit := &deposit.Deposit{
		OutPoint:           depositOutpoint,
		ConfirmationHeight: 0,
	}
	confirmationHeight := int64(mockLnd.Height) - deposit.MinConfs + 1
	freshDeposit := &deposit.Deposit{
		OutPoint:           depositOutpoint,
		ConfirmationHeight: confirmationHeight,
	}
	type depositLookup struct {
		outpoints     []string
		ignoreUnknown bool
	}
	depositLookups := make(chan depositLookup, 1)
	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        2_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now().Add(-time.Hour),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 1,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{staleDeposit},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier: mockLnd.ChainNotifier,
		DepositManager: &noopDepositManager{
			deposits: []*deposit.Deposit{freshDeposit},
			depositsForOutpoints: func(outpoints []string,
				ignoreUnknown bool) {

				select {
				case depositLookups <- depositLookup{
					outpoints: append(
						[]string(nil), outpoints...,
					),
					ignoreUnknown: ignoreUnknown,
				}:
				default:
				}
			},
		},
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height))

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice canceled immediately after deposit "+
			"confirmation: %v", hash)

	case <-time.After(200 * time.Millisecond):
	}

	select {
	case lookup := <-depositLookups:
		require.Equal(t, []string{
			depositOutpoint.String(),
		}, lookup.outpoints)
		require.False(t, lookup.ignoreUnknown)

	case <-ctx.Done():
		t.Fatalf("deposit refresh was not called: %v", ctx.Err())
	}

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestLegacyConfirmationFallbackStopsOnFreshnessFailure verifies that MinConfs
// is not evaluated from cached deposits when the wallet reconciliation fails.
func TestLegacyConfirmationFallbackStopsOnFreshnessFailure(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{14},
		Index: 0,
	}
	lookupCalled := false
	depositManager := &noopDepositManager{
		deposits: []*deposit.Deposit{{
			OutPoint:           outpoint,
			ConfirmationHeight: 1,
		}},
		ensureFreshErr: errors.New("wallet unavailable"),
		depositsForOutpoints: func([]string, bool) {
			lookupCalled = true
		},
	}
	f := &FSM{
		cfg: &Config{
			DepositManager: depositManager,
		},
		loopIn: &StaticAddressLoopIn{
			DepositOutpoints: []string{outpoint.String()},
		},
	}

	reached := f.shouldStartLegacyConfirmationFallback(
		t.Context(), deposit.MinConfs,
	)
	require.False(t, reached)
	require.False(t, lookupCalled)
}

// TestMonitorInvoiceAndHtlcTxUnlocksOnHtlcTimeoutWithoutDeadline verifies that
// deposits are unlocked even if the payment deadline never started before the
// HTLC timeout path opened.
func TestMonitorInvoiceAndHtlcTxUnlocksOnHtlcTimeoutWithoutDeadline(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{10, 11, 12}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{10},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        mockLnd.Height,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height+1))

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnSwapTimedOut, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Equal(t, []fsm.EventType{fsm.OnError}, depositMgr.events)
	require.Equal(t, []fsm.StateType{deposit.Deposited}, depositMgr.states)
	require.Equal(t, []lntypes.Hash{swapHash}, depositMgr.releasedFor)
}

// TestMonitorInvoiceAndHtlcTxTimeoutKeepsTakenDeposit checks that the release
// at the htlc expiry leaves a deposit alone that another swap took after this
// swap released it.
func TestMonitorInvoiceAndHtlcTxTimeoutKeepsTakenDeposit(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	swapHash := lntypes.Hash{10, 11, 12}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{10},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		SwapInvoice:           testSwapInvoice,
		HtlcCltvExpiry:        mockLnd.Height,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		PaymentTimeoutSeconds: 3_600,
		DepositOutpoints: []string{
			depositOutpoint.String(),
		},
		Deposits: []*deposit.Deposit{{
			OutPoint: depositOutpoint,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: mockLnd.LndServices.Invoices,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)

	// Another swap took the deposit after this swap released it.
	otherSwap := lntypes.Hash{13, 14, 15}
	taken := depositMgr.active[depositOutpoint]
	taken.SetState(deposit.LoopingIn)
	taken.SwapHash = &otherSwap

	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height+1))

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnSwapTimedOut, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Empty(t, depositMgr.events)
	require.Equal(t, []lntypes.Hash{swapHash}, depositMgr.releasedFor)
	require.True(t, taken.IsInState(deposit.LoopingIn))
	require.Equal(t, &otherSwap, taken.SwapHash)
}

// TestMonitorInvoiceAndHtlcTxDoesNotAdvanceWhenTimeoutDepositTransitionFails
// verifies that a failed deposit timeout transition keeps the loop-in in its
// recoverable monitor state.
func TestMonitorInvoiceAndHtlcTxDoesNotAdvanceWhenTimeoutDepositTransitionFails(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{20, 21, 22}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractCanceled,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	f.loopIn.HtlcCltvExpiry = mockLnd.Height
	depositMgr.err = errors.New("transition failed")
	htlcConf := htlcConfirmation(t, f)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	var confRegistration *test.ConfRegistration
	select {
	case confRegistration = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	confRegistration.ConfChan <- htlcConf

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.OnSweepingHtlcTimeout, transition.event)
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposit timeout transition was not attempted: %v",
			ctx.Err())
	}

	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height+1))

	select {
	case event := <-resultChan:
		require.Equal(t, OnRecover, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}
}

// TestMonitorInvoiceAndHtlcTxRetriesOnlyPendingTimeoutDeposits verifies that a
// retry after a partial timeout transition skips deposits that already reached
// the target state.
func TestMonitorInvoiceAndHtlcTxRetriesOnlyPendingTimeoutDeposits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{26, 27, 28}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractCanceled,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	f.loopIn.HtlcCltvExpiry = mockLnd.Height

	firstDeposit := f.loopIn.Deposits[0]
	secondDeposit := &deposit.Deposit{
		OutPoint: wire.OutPoint{Index: 1},
		Value:    300_000,
	}
	firstDeposit.SetState(deposit.LoopingIn)
	secondDeposit.SetState(deposit.LoopingIn)
	f.loopIn.Deposits = append(f.loopIn.Deposits, secondDeposit)
	depositMgr.active[secondDeposit.OutPoint] = secondDeposit

	attempts := 0
	depositMgr.transition = func(deposits []*deposit.Deposit,
		_ fsm.EventType, state fsm.StateType) error {

		attempts++
		if attempts == 1 {
			deposits[0].SetState(state)

			return errors.New("partial transition")
		}
		for _, d := range deposits {
			d.SetState(state)
		}

		return nil
	}
	depositMgr.transitionChan = make(chan depositTransition, 2)
	htlcConf := htlcConfirmation(t, f)

	runMonitor := func(runCtx context.Context) chan fsm.EventType {
		resultChan := make(chan fsm.EventType, 1)
		go func() {
			resultChan <- f.MonitorInvoiceAndHtlcTxAction(runCtx, nil)
		}()

		select {
		case <-mockLnd.SingleInvoiceSubcribeChannel:
		case <-runCtx.Done():
			t.Fatalf("invoice subscription not registered: %v",
				runCtx.Err())
		}

		var confRegistration *test.ConfRegistration
		select {
		case confRegistration = <-mockLnd.RegisterConfChannel:
		case <-runCtx.Done():
			t.Fatalf("htlc conf registration not received: %v",
				runCtx.Err())
		}
		confRegistration.ConfChan <- htlcConf

		return resultChan
	}

	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstResult := runMonitor(firstCtx)
	select {
	case event := <-firstResult:
		require.Equal(t, OnRecover, event)
	case <-ctx.Done():
		t.Fatalf("first monitor attempt did not exit: %v", ctx.Err())
	}
	cancelFirst()
	firstTransition := <-depositMgr.transitionChan

	require.True(t, firstDeposit.IsInState(deposit.SweepHtlcTimeout))
	require.True(t, secondDeposit.IsInState(deposit.LoopingIn))

	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	secondResult := runMonitor(secondCtx)
	secondTransition := <-depositMgr.transitionChan
	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height+1))

	select {
	case event := <-secondResult:
		require.Equal(t, OnSweepHtlcTimeout, event)
	case <-ctx.Done():
		t.Fatalf("second monitor attempt did not exit: %v", ctx.Err())
	}

	require.Equal(t, []*deposit.Deposit{
		firstDeposit, secondDeposit,
	}, firstTransition.deposits)
	require.Equal(t, []*deposit.Deposit{
		secondDeposit,
	}, secondTransition.deposits)
	require.True(t, firstDeposit.IsInState(deposit.SweepHtlcTimeout))
	require.True(t, secondDeposit.IsInState(deposit.SweepHtlcTimeout))
}

// TestMonitorInvoiceAndHtlcTxDoesNotFailWhenTimeoutUnlockFails verifies that a
// failed unlock does not make the loop-in terminal while deposits remain
// locked.
func TestMonitorInvoiceAndHtlcTxDoesNotFailWhenTimeoutUnlockFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{23, 24, 25}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	f.loopIn.HtlcCltvExpiry = mockLnd.Height
	depositMgr.err = errors.New("transition failed")

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	waitForMonitorSubscriptions(t, ctx, mockLnd)
	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height+1))

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnRecover, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Equal(t, []fsm.EventType{fsm.OnError}, depositMgr.events)
	require.Equal(t, []fsm.StateType{deposit.Deposited}, depositMgr.states)
}

// waitForMonitorSubscriptions waits until invoice and HTLC watchers are active.
func waitForMonitorSubscriptions(t *testing.T, ctx context.Context,
	mockLnd *test.LndMockServices) {

	t.Helper()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	select {
	case <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}
}

// newInvoiceMonitorTestFSM creates the minimal monitor-state setup shared by
// invoice precedence and cancellation tests.
func newInvoiceMonitorTestFSM(t *testing.T, ctx context.Context,
	mockLnd *test.LndMockServices, swapHash lntypes.Hash,
	decision ConfirmationRiskDecision,
	invoicesClient lndclient.InvoicesClient) (*FSM, *recordingDepositManager) {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		SwapHash:                     swapHash,
		SwapInvoice:                  testSwapInvoice,
		HtlcCltvExpiry:               mockLnd.Height + 1_000,
		InitiationHeight:             uint32(mockLnd.Height),
		InitiationTime:               time.Now(),
		ProtocolVersion:              version.ProtocolVersion_V0,
		ClientPubkey:                 clientKey.PubKey(),
		ServerPubkey:                 serverKey.PubKey(),
		PaymentTimeoutSeconds:        3_600,
		ConfirmationRiskDecision:     decision,
		ConfirmationRiskDecisionTime: time.Now(),
		Deposits: []*deposit.Deposit{{
			Value: 200_000,
		}},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	depositMgr := newRecordingDepositManager(loopIn.Deposits...)
	depositMgr.transitionChan = make(chan depositTransition, 1)
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientKey.PubKey(),
				ServerPubkey:    serverKey.PubKey(),
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:  mockLnd.ChainNotifier,
		DepositManager: depositMgr,
		InvoicesClient: invoicesClient,
		LndClient:      mockLnd.Client,
		ChainParams:    mockLnd.ChainParams,
	}

	f, err := NewFSM(ctx, loopIn, cfg, true)
	require.NoError(t, err)

	return f, depositMgr
}

// htlcConfirmation returns a confirmation of an htlc tx that spends the
// deposits the loop-in was signed for and pays to its htlc.
func htlcConfirmation(t *testing.T, f *FSM) *chainntnfs.TxConfirmation {
	t.Helper()

	htlc, err := f.loopIn.getHtlc(f.cfg.ChainParams)
	require.NoError(t, err)

	outpoints, err := f.htlcTxInputs()
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	for _, outpoint := range outpoints {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	}
	tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: htlc.PkScript})

	return &chainntnfs.TxConfirmation{Tx: tx}
}

// TestMonitorInvoiceAndHtlcTxIgnoresDecoyHtlcConf verifies that a confirmation
// of a tx that pays to the htlc script but doesn't spend the deposits doesn't
// lock the deposits for the htlc timeout sweep. The htlc confirmation is
// registered by pkScript only, so lnd reports the first tx paying to the htlc
// script, which the server can create from its own funds. The monitor then
// watches the deposit spend and confirms the htlc tx by its txid. A deposit
// spend by a tx that isn't the htlc tx doesn't confirm the htlc either.
func TestMonitorInvoiceAndHtlcTxIgnoresDecoyHtlcConf(t *testing.T) {
	// dustDecoy pays a dust output to the htlc from an input the server
	// controls, without spending the deposits.
	dustDecoy := func(htlcPkScript []byte) *chainntnfs.TxConfirmation {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{0xde, 0xad},
			},
		})
		tx.AddTxOut(&wire.TxOut{Value: 330, PkScript: htlcPkScript})

		return &chainntnfs.TxConfirmation{Tx: tx}
	}

	tests := []struct {
		name  string
		decoy func(htlcPkScript []byte) *chainntnfs.TxConfirmation

		// spentByHtlc is whether the htlc tx spends the deposits,
		// instead of a tx that doesn't pay to the htlc.
		spentByHtlc bool
	}{
		{
			name:        "dust output from foreign input",
			decoy:       dustDecoy,
			spentByHtlc: true,
		},
		{
			name: "nil confirmation",
			decoy: func([]byte) *chainntnfs.TxConfirmation {
				return nil
			},
			spentByHtlc: true,
		},
		{
			name:  "deposits spent by other tx",
			decoy: dustDecoy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testMonitorIgnoresDecoyHtlcConf(
				t, tc.decoy, tc.spentByHtlc,
			)
		})
	}
}

// testMonitorIgnoresDecoyHtlcConf delivers the decoy confirmation and then the
// deposit spend, and checks that only the htlc tx locks the deposits.
func testMonitorIgnoresDecoyHtlcConf(t *testing.T,
	decoy func(htlcPkScript []byte) *chainntnfs.TxConfirmation,
	spentByHtlc bool) {

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{32, 33, 34}

	// With the invoice canceled, an htlc confirmation locks the deposits
	// for the timeout sweep right away.
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractCanceled,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	depositMgr.transitionChan = make(chan depositTransition, 4)

	dep := f.loopIn.Deposits[0]
	dep.OutPoint = wire.OutPoint{Hash: chainhash.Hash{7}, Index: 1}
	dep.SetState(deposit.LoopingIn)
	depositMgr.active = map[wire.OutPoint]*deposit.Deposit{
		dep.OutPoint: dep,
	}
	f.loopIn.DepositOutpoints = []string{dep.OutPoint.String()}
	f.loopIn.AddressParams = &script.Parameters{
		PkScript: []byte{0x51, 0x20, 0x01},
	}

	htlc, err := f.loopIn.getHtlc(f.cfg.ChainParams)
	require.NoError(t, err)
	htlcConf := htlcConfirmation(t, f)
	htlcTxid := htlcConf.Tx.TxHash()

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	var confReg *test.ConfRegistration
	select {
	case confReg = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}
	require.Nil(t, confReg.TxID)

	confReg.ConfChan <- decoy(htlc.PkScript)

	// The monitor must ignore the decoy and watch the deposit spend
	// instead of locking the deposits.
	var spendReg *test.SpendRegistration
	select {
	case transition := <-depositMgr.transitionChan:
		t.Fatalf("decoy confirmation moved deposits to %v",
			transition.state)

	case spendReg = <-mockLnd.RegisterSpendChannel:

	case <-ctx.Done():
		t.Fatalf("deposit spend registration not received: %v",
			ctx.Err())
	}
	require.Equal(t, dep.OutPoint, *spendReg.Outpoint)
	require.Equal(t, f.loopIn.AddressParams.PkScript, spendReg.PkScript)

	if !spentByHtlc {
		otherTx := wire.NewMsgTx(2)
		otherTx.AddTxIn(&wire.TxIn{PreviousOutPoint: dep.OutPoint})
		otherTx.AddTxOut(&wire.TxOut{
			Value:    100_000,
			PkScript: []byte{0x51},
		})
		spendReg.SpendChannel <- &chainntnfs.SpendDetail{
			SpendingTx: otherTx,
		}

		select {
		case reg := <-mockLnd.RegisterConfChannel:
			t.Fatalf("conf registered for tx %v that isn't the "+
				"htlc tx", reg.TxID)

		case transition := <-depositMgr.transitionChan:
			t.Fatalf("deposit spend moved deposits to %v",
				transition.state)

		case <-time.After(200 * time.Millisecond):
		}

		cancel()
		require.Equal(t, fsm.NoOp, <-resultChan)

		return
	}

	// The htlc tx spends the deposit, and its confirmation is then
	// registered by txid.
	spendReg.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx: htlcConf.Tx,
	}

	select {
	case confReg = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc txid conf registration not received: %v",
			ctx.Err())
	}
	require.NotNil(t, confReg.TxID)
	require.Equal(t, htlcTxid, *confReg.TxID)
	require.Empty(t, depositMgr.transitionChan)

	confReg.ConfChan <- htlcConf

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposits were not locked after the htlc "+
			"confirmation: %v", ctx.Err())
	}

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.NoOp, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// decoyMonitorTest runs the monitor of a swap with a canceled invoice and one
// deposit. The tests deliver a decoy confirmation and then drive the deposit
// spend watch.
type decoyMonitorTest struct {
	t          *testing.T
	ctx        context.Context
	mockLnd    *test.LndMockServices
	f          *FSM
	depositMgr *recordingDepositManager
	txOuts     *recordingTxOutChecker
	resultChan chan fsm.EventType
}

func newDecoyMonitorTest(t *testing.T, ctx context.Context) *decoyMonitorTest {
	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{32, 33, 35}

	// With the invoice canceled, an htlc confirmation locks the deposits
	// for the timeout sweep right away.
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractCanceled,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	depositMgr.transitionChan = make(chan depositTransition, 4)

	dep := f.loopIn.Deposits[0]
	dep.OutPoint = wire.OutPoint{Hash: chainhash.Hash{7}, Index: 1}
	dep.SetState(deposit.LoopingIn)
	depositMgr.active = map[wire.OutPoint]*deposit.Deposit{
		dep.OutPoint: dep,
	}
	f.loopIn.DepositOutpoints = []string{dep.OutPoint.String()}
	f.loopIn.AddressParams = &script.Parameters{
		PkScript: []byte{0x51, 0x20, 0x01},
	}

	// The htlc expires two blocks from now.
	f.loopIn.HtlcCltvExpiry = mockLnd.Height + 2

	txOuts := &recordingTxOutChecker{}
	f.cfg.TxOutChecker = txOuts

	return &decoyMonitorTest{
		t:          t,
		ctx:        ctx,
		mockLnd:    mockLnd,
		f:          f,
		depositMgr: depositMgr,
		txOuts:     txOuts,
		resultChan: make(chan fsm.EventType, 1),
	}
}

// htlcTx returns a fee variant of the htlc tx. Every variant spends the same
// deposits and pays to the htlc, the output value makes the txid differ.
func (d *decoyMonitorTest) htlcTx(value int64) *wire.MsgTx {
	tx := htlcConfirmation(d.t, d.f).Tx
	tx.TxOut[0].Value = value

	return tx
}

// start runs the monitor and returns the htlc confirmation registration.
func (d *decoyMonitorTest) start() *test.ConfRegistration {
	go func() {
		d.resultChan <- d.f.MonitorInvoiceAndHtlcTxAction(d.ctx, nil)
	}()

	select {
	case <-d.mockLnd.SingleInvoiceSubcribeChannel:
	case <-d.ctx.Done():
		d.t.Fatalf("invoice subscription not registered: %v",
			d.ctx.Err())
	}

	reg := d.confRegistration()
	require.Nil(d.t, reg.TxID)

	return reg
}

// confRegistration waits for the next htlc confirmation registration.
func (d *decoyMonitorTest) confRegistration() *test.ConfRegistration {
	select {
	case reg := <-d.mockLnd.RegisterConfChannel:
		return reg

	case event := <-d.resultChan:
		d.t.Fatalf("monitor exited with %v: %v", event,
			d.f.LastActionError)

	case <-d.ctx.Done():
		d.t.Fatalf("htlc conf registration not received: %v",
			d.ctx.Err())
	}

	return nil
}

// spendRegistration waits for the next deposit spend registration.
func (d *decoyMonitorTest) spendRegistration() *test.SpendRegistration {
	select {
	case reg := <-d.mockLnd.RegisterSpendChannel:
		require.NotNil(d.t, reg.ReorgChan)

		return reg

	case event := <-d.resultChan:
		d.t.Fatalf("monitor exited with %v: %v", event,
			d.f.LastActionError)

	case <-d.ctx.Done():
		d.t.Fatalf("deposit spend registration not received: %v",
			d.ctx.Err())
	}

	return nil
}

// decoy confirms a dust output to the htlc that doesn't spend the deposits
// and returns the deposit spend registration that follows.
func (d *decoyMonitorTest) decoy(
	reg *test.ConfRegistration) *test.SpendRegistration {

	htlc, err := d.f.loopIn.getHtlc(d.f.cfg.ChainParams)
	require.NoError(d.t, err)

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{0xde, 0xad},
		},
	})
	tx.AddTxOut(&wire.TxOut{Value: 330, PkScript: htlc.PkScript})
	reg.ConfChan <- &chainntnfs.TxConfirmation{Tx: tx}

	return d.spendRegistration()
}

// spend reports the tx as the spender of the watched deposit and returns the
// confirmation registration for its txid.
func (d *decoyMonitorTest) spend(reg *test.SpendRegistration,
	tx *wire.MsgTx) *test.ConfRegistration {

	reg.SpendChannel <- &chainntnfs.SpendDetail{SpendingTx: tx}

	confReg := d.confRegistration()
	require.NotNil(d.t, confReg.TxID)
	require.Equal(d.t, tx.TxHash(), *confReg.TxID)

	return confReg
}

// confirm delivers the confirmation of the htlc tx and waits for the deposits
// to be locked for the timeout sweep.
func (d *decoyMonitorTest) confirm(reg *test.ConfRegistration,
	tx *wire.MsgTx) {

	reg.ConfChan <- &chainntnfs.TxConfirmation{Tx: tx}

	select {
	case transition := <-d.depositMgr.transitionChan:
		require.Equal(
			d.t, deposit.SweepHtlcTimeout, transition.state,
		)

	case <-d.ctx.Done():
		d.t.Fatalf("deposits were not locked after the htlc "+
			"confirmation: %v", d.ctx.Err())
	}
}

// expire notifies a block at which the htlc has timed out.
func (d *decoyMonitorTest) expire() {
	height := max(d.mockLnd.Height, d.f.loopIn.HtlcCltvExpiry) + 1
	require.NoError(d.t, d.mockLnd.NotifyHeight(height))
}

// requireWaiting checks that the monitor keeps running and didn't touch the
// deposits.
func (d *decoyMonitorTest) requireWaiting() {
	select {
	case event := <-d.resultChan:
		d.t.Fatalf("monitor exited with %v: %v", event,
			d.f.LastActionError)

	case transition := <-d.depositMgr.transitionChan:
		d.t.Fatalf("deposits moved to %v", transition.state)

	case <-time.After(200 * time.Millisecond):
	}
}

// result waits for the monitor to exit.
func (d *decoyMonitorTest) result() fsm.EventType {
	select {
	case event := <-d.resultChan:
		return event

	case <-d.ctx.Done():
		d.t.Fatalf("monitor did not exit: %v", d.ctx.Err())
	}

	return fsm.NoOp
}

// requireTimeoutSweep expires the htlc and checks that the swap sweeps it and
// that the deposits were never released.
func (d *decoyMonitorTest) requireTimeoutSweep() {
	d.expire()
	require.Equal(d.t, OnSweepHtlcTimeout, d.result())
	require.NotContains(d.t, d.depositMgr.states, deposit.Deposited)
}

// requireTimedOut expires the htlc and checks that the swap times out and
// releases the deposits.
func (d *decoyMonitorTest) requireTimedOut() {
	d.expire()
	require.Equal(d.t, OnSwapTimedOut, d.result())
	require.Equal(
		d.t, []fsm.StateType{deposit.Deposited}, d.depositMgr.states,
	)
}

// TestMonitorHtlcTimeoutWaitsForSpentDeposits verifies that the htlc timeout
// doesn't release deposits the htlc tx already spent. After a decoy the htlc
// tx is found from the deposit spend, and its confirmation can arrive after
// the htlc expiry.
func TestMonitorHtlcTimeoutWaitsForSpentDeposits(t *testing.T) {
	// The spend watch reports the htlc tx before the htlc expires, and
	// lnd's wallet isn't asked.
	t.Run("spend seen by the monitor", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.f.cfg.TxOutChecker = nil
		htlcTx := d.htlcTx(100_000)

		spendReg := d.decoy(d.start())
		confReg := d.spend(spendReg, htlcTx)

		d.expire()
		d.requireWaiting()

		d.confirm(confReg, htlcTx)
		d.requireTimeoutSweep()
	})

	// The monitor starts after the htlc expiry, so the block arrives
	// before the deposit spend. lnd's wallet lists the htlc tx.
	t.Run("start after htlc expiry", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.f.loopIn.HtlcCltvExpiry = d.mockLnd.Height - 10
		d.txOuts.htlcSpent = true
		htlcTx := d.htlcTx(100_000)

		confReg := d.start()
		d.requireWaiting()

		spendReg := d.decoy(confReg)
		confReg = d.spend(spendReg, htlcTx)
		d.requireWaiting()

		d.confirm(confReg, htlcTx)
		d.requireTimeoutSweep()

		// The lookup asked about the signed deposits and the htlc.
		htlc, err := d.f.loopIn.getHtlc(d.f.cfg.ChainParams)
		require.NoError(t, err)
		signedOutpoints, err := d.f.depositOutpoints()
		require.NoError(t, err)
		require.NotEmpty(t, d.txOuts.spendOutpoints)
		require.Equal(t, signedOutpoints, d.txOuts.spendOutpoints[0])
		require.Equal(t, htlc.PkScript, d.txOuts.spendPkScripts[0])
	})

	// lnd's wallet lists the htlc tx at the htlc expiry and no longer
	// lists it at the next block. The swap asks again and stops waiting.
	t.Run("htlc tx gone at a later block", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.f.loopIn.HtlcCltvExpiry = d.mockLnd.Height - 10
		d.txOuts.htlcSpentResults = []bool{true}

		d.start()
		d.requireWaiting()

		d.requireTimedOut()
		require.Len(t, d.txOuts.spendOutpoints, 2)
	})

	// A swap without an outpoint snapshot asks about its deposit rows.
	t.Run("no outpoint snapshot", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.f.loopIn.DepositOutpoints = nil
		d.f.loopIn.HtlcCltvExpiry = d.mockLnd.Height - 10
		d.txOuts.htlcSpentResults = []bool{true}

		d.start()
		d.requireWaiting()

		// Let the monitor exit before reading what it asked about.
		d.requireTimedOut()
		require.NotEmpty(t, d.txOuts.spendOutpoints)
		require.Equal(
			t, d.f.loopIn.Outpoints(), d.txOuts.spendOutpoints[0],
		)
	})

	// A failed lookup must not release the deposits.
	t.Run("lookup error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.f.loopIn.HtlcCltvExpiry = d.mockLnd.Height - 10
		d.txOuts.err = errors.New("lnd unavailable")

		d.start()
		require.Equal(t, OnRecover, d.result())
		require.Empty(t, d.depositMgr.states)
	})

	// The htlc tx didn't spend the deposits, so they are released.
	t.Run("deposits not spent", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)

		d.decoy(d.start())
		d.requireTimedOut()
		require.NotEmpty(t, d.txOuts.spendOutpoints)
	})
}

// TestMonitorDropsHtlcTxOnReorg verifies that a reorg drops the htlc tx found
// from the deposit spend. The deposits can then be spent by another signed
// fee variant of the htlc tx, or not at all.
func TestMonitorDropsHtlcTxOnReorg(t *testing.T) {
	// The spend is reorged out and another fee variant of the htlc tx
	// spends the deposits.
	t.Run("spend reorg, other fee variant", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		variantA, variantB := d.htlcTx(100_000), d.htlcTx(99_000)
		require.NotEqual(t, variantA.TxHash(), variantB.TxHash())

		spendReg := d.decoy(d.start())
		confReg := d.spend(spendReg, variantA)

		// The reorg starts a new spend watch for the same deposit.
		spendReg.ReorgChan <- struct{}{}
		spendReg2 := d.spendRegistration()
		require.Equal(t, *spendReg.Outpoint, *spendReg2.Outpoint)
		require.Equal(t, spendReg.PkScript, spendReg2.PkScript)

		// The txid of variant A is dropped, so a confirmation
		// subscription that fails now registers by script again.
		confReg.ErrChan <- errors.New("lnd restarting")
		confReg = d.confRegistration()
		require.Nil(t, confReg.TxID)

		confReg = d.spend(spendReg2, variantB)
		d.confirm(confReg, variantB)
		d.requireTimeoutSweep()
	})

	// The spend is reorged out and the same htlc tx confirms again after
	// the htlc expiry. lnd's wallet still lists it, so the deposits stay
	// locked.
	t.Run("spend reorg, htlc tx confirms again", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.txOuts.htlcSpent = true
		htlcTx := d.htlcTx(100_000)

		spendReg := d.decoy(d.start())
		d.spend(spendReg, htlcTx)

		spendReg.ReorgChan <- struct{}{}
		spendReg = d.spendRegistration()

		d.expire()
		d.requireWaiting()

		confReg := d.spend(spendReg, htlcTx)
		d.confirm(confReg, htlcTx)
		d.requireTimeoutSweep()
		require.NotEmpty(t, d.txOuts.spendOutpoints)
	})

	// The spend is reorged out and the htlc tx is gone for good. The swap
	// must not wait for it forever.
	t.Run("spend reorg, htlc tx gone", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)

		spendReg := d.decoy(d.start())
		d.spend(spendReg, d.htlcTx(100_000))

		spendReg.ReorgChan <- struct{}{}
		d.spendRegistration()

		d.requireTimedOut()
		require.NotEmpty(t, d.txOuts.spendOutpoints)
	})

	// The spend is reorged out after the htlc tx confirmed. The
	// confirmation no longer counts.
	t.Run("spend reorg after confirmation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		htlcTx := d.htlcTx(100_000)

		// With an open invoice the confirmation doesn't lock the
		// deposits yet.
		d.mockLnd.SetInvoice(&lndclient.Invoice{
			Hash:  d.f.loopIn.SwapHash,
			State: invoices.ContractOpen,
		})

		spendReg := d.decoy(d.start())
		confReg := d.spend(spendReg, htlcTx)
		confReg.ConfChan <- &chainntnfs.TxConfirmation{Tx: htlcTx}

		// The monitor must take the confirmation before the reorg.
		require.Eventually(t, func() bool {
			return len(confReg.ConfChan) == 0
		}, testTimeout, 10*time.Millisecond)
		d.requireWaiting()

		spendReg.ReorgChan <- struct{}{}
		d.spendRegistration()

		d.requireTimedOut()
	})

	// A replaced spend registration has its own reorg channel, so a
	// reorg it reports later is not handled a second time.
	t.Run("reorg on a replaced registration", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)

		spendReg := d.decoy(d.start())
		d.spend(spendReg, d.htlcTx(100_000))

		spendReg.ReorgChan <- struct{}{}
		d.spendRegistration()

		spendReg.ReorgChan <- struct{}{}
		select {
		case <-d.mockLnd.RegisterSpendChannel:
			t.Fatal("reorg of a replaced registration was handled")

		case <-time.After(200 * time.Millisecond):
		}

		d.requireTimedOut()
	})

	// A reorg reported by the confirmation subscription drops the txid
	// too, so the confirmation is registered by script again.
	t.Run("confirmation reorg", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)

		spendReg := d.decoy(d.start())
		confReg := d.spend(spendReg, d.htlcTx(100_000))

		require.NotNil(t, confReg.ReorgChan)
		confReg.ReorgChan <- struct{}{}
		confReg = d.confRegistration()
		require.Nil(t, confReg.TxID)

		d.requireTimedOut()
	})
}

// TestMonitorDepositSpendWatchErrors verifies that the monitor is retried if
// the deposit spend can't be watched.
func TestMonitorDepositSpendWatchErrors(t *testing.T) {
	t.Run("subscription error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)

		spendReg := d.decoy(d.start())
		spendReg.ErrChan <- errors.New("lnd restarting")

		require.Equal(t, OnRecover, d.result())
		require.Empty(t, d.depositMgr.states)
	})

	t.Run("registration error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()

		d := newDecoyMonitorTest(t, ctx)
		d.f.loopIn.AddressParams = nil

		confReg := d.start()
		confReg.ConfChan <- &chainntnfs.TxConfirmation{
			Tx: wire.NewMsgTx(2),
		}

		require.Equal(t, OnRecover, d.result())
		require.Empty(t, d.depositMgr.states)
	})
}

// TestMonitorWatchesSignedDepositOutpoints verifies that the monitor takes the
// htlc tx inputs from the outpoints the htlc tx was signed for. The deposit
// rows of a recovered swap can differ from them.
func TestMonitorWatchesSignedDepositOutpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	d := newDecoyMonitorTest(t, ctx)

	// The swap was signed for two deposits, but only the second one is
	// still mapped to it.
	first := wire.OutPoint{Hash: chainhash.Hash{6}, Index: 3}
	second := d.f.loopIn.Deposits[0].OutPoint
	d.f.loopIn.DepositOutpoints = []string{
		first.String(), second.String(),
	}

	htlcTx := d.htlcTx(100_000)
	require.Len(t, htlcTx.TxIn, 2)

	spendReg := d.decoy(d.start())
	require.Equal(t, first, *spendReg.Outpoint)

	confReg := d.spend(spendReg, htlcTx)
	d.confirm(confReg, htlcTx)
	d.requireTimeoutSweep()
}

// TestIsHtlcTx verifies that only a tx that spends exactly the deposits of the
// swap and pays to the htlc is taken as the htlc tx.
func TestIsHtlcTx(t *testing.T) {
	htlcPkScript := []byte{0x00, 0x20, 0x01}
	deposit1 := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	deposit2 := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1}
	foreign := wire.OutPoint{Hash: chainhash.Hash{3}, Index: 0}

	f := &FSM{
		loopIn: &StaticAddressLoopIn{
			Deposits: []*deposit.Deposit{
				{OutPoint: deposit1},
				{OutPoint: deposit2},
			},
		},
	}

	newTx := func(pkScript []byte, inputs ...wire.OutPoint) *wire.MsgTx {
		tx := wire.NewMsgTx(2)
		for _, input := range inputs {
			tx.AddTxIn(&wire.TxIn{PreviousOutPoint: input})
		}
		tx.AddTxOut(&wire.TxOut{Value: 1_000, PkScript: pkScript})

		return tx
	}

	tests := []struct {
		name string
		tx   *wire.MsgTx
		want bool
	}{
		{
			name: "htlc tx",
			tx:   newTx(htlcPkScript, deposit1, deposit2),
			want: true,
		},
		{
			name: "deposits in other order",
			tx:   newTx(htlcPkScript, deposit2, deposit1),
			want: true,
		},
		{
			name: "nil tx",
		},
		{
			name: "foreign input only",
			tx:   newTx(htlcPkScript, foreign),
		},
		{
			name: "one deposit missing",
			tx:   newTx(htlcPkScript, deposit1),
		},
		{
			name: "foreign input instead of a deposit",
			tx:   newTx(htlcPkScript, deposit1, foreign),
		},
		{
			name: "extra foreign input",
			tx:   newTx(htlcPkScript, deposit1, deposit2, foreign),
		},
		{
			name: "same deposit twice",
			tx:   newTx(htlcPkScript, deposit1, deposit1),
		},
		{
			name: "no htlc output",
			tx:   newTx([]byte{0x51}, deposit1, deposit2),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := f.isHtlcTx(tc.tx, htlcPkScript)
			require.Equal(t, tc.want, got)
		})
	}

	// The htlc tx is signed for the outpoint snapshot. The deposit rows
	// of a recovered swap can differ from it and must not be used then.
	t.Run("snapshot differs from deposit rows", func(t *testing.T) {
		f := &FSM{
			loopIn: &StaticAddressLoopIn{
				DepositOutpoints: []string{
					deposit1.String(), deposit2.String(),
				},
				Deposits: []*deposit.Deposit{
					{OutPoint: deposit1},
				},
			},
		}

		require.True(t, f.isHtlcTx(
			newTx(htlcPkScript, deposit1, deposit2), htlcPkScript,
		))
		require.False(t, f.isHtlcTx(
			newTx(htlcPkScript, deposit1), htlcPkScript,
		))
	})
}

// failingCancelInvoices records cancellation attempts and returns a configured
// error after its release channel is closed.
type failingCancelInvoices struct {
	lndclient.InvoicesClient

	cancelCalls chan lntypes.Hash
	release     chan struct{}
	err         error
}

// flakySubscribeInvoices counts subscription attempts and returns a configured
// subscription error.
type flakySubscribeInvoices struct {
	lndclient.InvoicesClient

	subscribeCalls int
	err            error
}

// firstLookupBarrier blocks the first invoice lookup until its release channel
// is closed.
type firstLookupBarrier struct {
	lndclient.LightningClient

	lookupStarted chan struct{}
	release       chan struct{}
	firstLookup   bool
}

func (f *firstLookupBarrier) LookupInvoice(ctx context.Context,
	hash lntypes.Hash) (*lndclient.Invoice, error) {

	invoice, err := f.LightningClient.LookupInvoice(ctx, hash)
	if f.firstLookup {
		return invoice, err
	}

	f.firstLookup = true
	close(f.lookupStarted)
	select {
	case <-f.release:
		return invoice, err

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *flakySubscribeInvoices) SubscribeSingleInvoice(ctx context.Context,
	hash lntypes.Hash) (<-chan lndclient.InvoiceUpdate, <-chan error, error) {

	f.subscribeCalls++
	if f.subscribeCalls == 1 {
		return nil, nil, f.err
	}

	return f.InvoicesClient.SubscribeSingleInvoice(ctx, hash)
}

func (f *failingCancelInvoices) CancelInvoice(ctx context.Context,
	hash lntypes.Hash) error {

	select {
	case f.cancelCalls <- hash:
	case <-ctx.Done():
		return ctx.Err()
	}

	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return f.err
}

// TestOriginalDepositOutpointUnavailableRequiresMissingTxOut verifies that a
// present txout does not trigger the RBF cancellation path.
func TestOriginalDepositOutpointUnavailableRequiresMissingTxOut(t *testing.T) {
	originalOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 0,
	}

	txOutChecker := &recordingTxOutChecker{
		txOuts: map[wire.OutPoint]*wire.TxOut{
			originalOutpoint: {Value: 10_000},
		},
	}
	f := &FSM{
		cfg: &Config{
			TxOutChecker: txOutChecker,
		},
		loopIn: &StaticAddressLoopIn{
			DepositOutpoints: []string{originalOutpoint.String()},
		},
	}

	unavailable, err := f.originalDepositOutpointUnavailable(t.Context())
	require.NoError(t, err)
	require.False(t, unavailable)
	require.Equal(t, [][]wire.OutPoint{{originalOutpoint}},
		txOutChecker.outpoints)
}

// TestSignHtlcTxActionCancelsWhenOriginalOutpointUnavailable verifies that a
// pending loop-in is canceled before HTLC signing if GetTxOuts reports that
// one of the originally selected outpoints is gone.
func TestSignHtlcTxActionCancelsWhenOriginalOutpointUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	swapHash := lntypes.Hash{9, 8, 7}
	originalOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:         swapHash,
		SwapInvoice:      testSwapInvoice,
		DepositOutpoints: []string{originalOutpoint.String()},
	}

	txOutChecker := &recordingTxOutChecker{}
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		InvoicesClient: mockLnd.LndServices.Invoices,
		TxOutChecker:   txOutChecker,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	event := f.SignHtlcTxAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, f.LastActionError, "original deposit outpoint no longer available",
	)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)
	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	require.Equal(t, [][]wire.OutPoint{{originalOutpoint}},
		txOutChecker.outpoints)
}

// TestSignHtlcTxActionDoesNotCancelOnTxOutLookupError verifies that lookup
// failures are treated as errors, but do not cancel the invoice. The invoice is
// only canceled when GetTxOuts omits an original outpoint.
func TestSignHtlcTxActionDoesNotCancelOnTxOutLookupError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	swapHash := lntypes.Hash{9, 8, 6}
	originalOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{3},
		Index: 0,
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:         swapHash,
		DepositOutpoints: []string{originalOutpoint.String()},
	}

	txOutChecker := &recordingTxOutChecker{
		err: errors.New("backend unavailable"),
	}
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		InvoicesClient: mockLnd.LndServices.Invoices,
		TxOutChecker:   txOutChecker,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	event := f.SignHtlcTxAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, f.LastActionError, "unable to get txout",
	)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		t.Fatalf("invoice should not have been canceled: %x", hash)
	default:
	}
}

// TestInitHtlcActionCancelsInvoiceOnServerError verifies that an invoice
// created before a server-side rejection is canceled immediately.
func TestInitHtlcActionCancelsInvoiceOnServerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	loopIn := &StaticAddressLoopIn{
		Deposits: []*deposit.Deposit{{
			Value: 200_000,
		}},
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		PaymentTimeoutSeconds: DefaultPaymentTimeoutSeconds,
		ProtocolVersion:       version.ProtocolVersion_V0,
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		DepositManager: &noopDepositManager{},
		WalletKit:      mockLnd.WalletKit,
		LndClient:      mockLnd.Client,
		InvoicesClient: mockLnd.LndServices.Invoices,
		Server: &initHtlcTestServer{
			loopInErr: errors.New("server rejected swap"),
		},
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	// The init step should fail and synchronously trigger deferred invoice
	// cleanup.
	event := f.InitHtlcAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, loopIn.SwapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}
}

// TestInitHtlcActionLocksDepositsForSwap checks that the deposits are locked
// and released with the swap's hash.
func TestInitHtlcActionLocksDepositsForSwap(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()

	dep := &deposit.Deposit{
		Value: 200_000,
	}
	dep.Hash = chainhash.Hash{0x21}
	dep.SetState(deposit.Deposited)
	loopIn := &StaticAddressLoopIn{
		Deposits:              []*deposit.Deposit{dep},
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		PaymentTimeoutSeconds: DefaultPaymentTimeoutSeconds,
		ProtocolVersion:       version.ProtocolVersion_V0,
	}

	depositMgr := newRecordingDepositManager(dep)
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		DepositManager: depositMgr,
		WalletKit:      mockLnd.WalletKit,
		LndClient:      mockLnd.Client,
		InvoicesClient: mockLnd.LndServices.Invoices,
		Server: &initHtlcTestServer{
			loopInErr: errors.New("server rejected swap"),
		},
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	event := f.InitHtlcAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.NotEqual(t, lntypes.Hash{}, loopIn.SwapHash)
	require.Equal(t, []lntypes.Hash{loopIn.SwapHash}, depositMgr.lockedFor)

	event = f.UnlockDepositsAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.Equal(
		t, []lntypes.Hash{loopIn.SwapHash}, depositMgr.releasedFor,
	)
	require.Equal(t, deposit.Deposited, dep.GetState())
}

// TestInitHtlcActionCancelsInvoiceOnFeeGuardFailure verifies that the early
// fee guard also cancels the pre-created invoice before returning an error.
func TestInitHtlcActionCancelsInvoiceOnFeeGuardFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		Deposits: []*deposit.Deposit{{
			Value: 200_000,
		}},
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		PaymentTimeoutSeconds: DefaultPaymentTimeoutSeconds,
		ProtocolVersion:       version.ProtocolVersion_V0,
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		DepositManager: &noopDepositManager{},
		WalletKit:      mockLnd.WalletKit,
		LndClient:      mockLnd.Client,
		InvoicesClient: mockLnd.LndServices.Invoices,
		Server: &initHtlcTestServer{
			loopInResp: &swapserverrpc.ServerStaticAddressLoopInResponse{
				HtlcServerPubKey: serverKey.PubKey().
					SerializeCompressed(),
				HtlcExpiry: mockLnd.Height +
					DefaultLoopInOnChainCltvDelta,
				StandardHtlcInfo: &swapserverrpc.ServerHtlcSigningInfo{
					FeeRate: 1_000_000,
				},
				HighFeeHtlcInfo: &swapserverrpc.ServerHtlcSigningInfo{},
				ExtremeFeeHtlcInfo: &swapserverrpc.
					ServerHtlcSigningInfo{},
			},
		},
		ValidateLoopInContract: func(int32, int32) error {
			return nil
		},
		MaxStaticAddrHtlcFeePercentage:       0,
		MaxStaticAddrHtlcBackupFeePercentage: 1,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	// The fee guard runs before persistence, so the deferred cleanup must
	// cancel the invoice on this error path as well.
	event := f.InitHtlcAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, loopIn.SwapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}
}

// TestUnlockDepositsActionCancelsInvoice verifies that stored swaps that enter
// the generic error unlock path also clean up their swap invoice.
func TestUnlockDepositsActionCancelsInvoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	dep := &deposit.Deposit{
		Value: 200_000,
	}
	swapHash := lntypes.Hash{0x44, 0x55}
	depositMgr := newRecordingDepositManager(dep)

	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			DepositManager: depositMgr,
			InvoicesClient: mockLnd.LndServices.Invoices,
		},
		loopIn: &StaticAddressLoopIn{
			SwapHash:    swapHash,
			SwapInvoice: "lnbc1test",
			Deposits:    []*deposit.Deposit{dep},
		},
	}

	event := f.UnlockDepositsAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.NoError(t, f.LastActionError)

	select {
	case hash := <-mockLnd.FailInvoiceChannel:
		require.Equal(t, swapHash, hash)

	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	require.Len(t, depositMgr.transitions, 1)
	require.Equal(t, []*deposit.Deposit{dep}, depositMgr.transitions[0].deposits)
	require.Equal(t, fsm.OnError, depositMgr.transitions[0].event)
	require.Equal(t, deposit.Deposited, depositMgr.transitions[0].state)
}

// TestUnlockDepositsActionReportsTransitionError ensures the unlock path
// preserves the real deposit transition failure for callers that need to log it.
func TestUnlockDepositsActionReportsTransitionError(t *testing.T) {
	dep := &deposit.Deposit{
		Value: 200_000,
	}
	depositMgr := newRecordingDepositManager(dep)
	depositMgr.err = errors.New("transition failed")
	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			DepositManager: depositMgr,
		},
		loopIn: &StaticAddressLoopIn{
			Deposits: []*deposit.Deposit{dep},
		},
	}

	event := f.UnlockDepositsAction(t.Context(), nil)
	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, f.LastActionError, "unable to unlock deposits",
	)
	require.ErrorContains(t, f.LastActionError, "transition failed")
}

// mockAddressManager is a minimal AddressManager implementation used by the
// test FSM setup.
type mockAddressManager struct {
	// params contains the address parameters returned to the test.
	params *script.Parameters

	// staticAddress contains the derived static address returned to the
	// test.
	staticAddress *script.StaticAddress
}

// GetStaticAddressParameters returns the configured address parameters.
func (m *mockAddressManager) GetStaticAddressParameters(_ context.Context) (
	*script.Parameters, error) {

	return m.params, nil
}

// GetStaticAddress returns the configured derived static address.
func (m *mockAddressManager) GetStaticAddress(_ context.Context) (
	*script.StaticAddress, error) {

	return m.staticAddress, nil
}

// noopDepositManager is a stub DepositManager used to satisfy FSM config.
type noopDepositManager struct {
	deposits             []*deposit.Deposit
	depositsForOutpoints func([]string, bool)
	depositErr           error
	ensureFreshErr       error
}

// EnsureDepositsFresh implements DepositManager with a no-op.
func (n *noopDepositManager) EnsureDepositsFresh(context.Context) error {
	return n.ensureFreshErr
}

// GetAllDeposits implements DepositManager with a no-op.
func (n *noopDepositManager) GetAllDeposits(_ context.Context) (
	[]*deposit.Deposit, error) {

	return nil, nil
}

// AllStringOutpointsActiveDeposits implements DepositManager with a no-op.
func (n *noopDepositManager) AllStringOutpointsActiveDeposits(
	_ []string, _ fsm.StateType) ([]*deposit.Deposit, bool) {

	return nil, false
}

// TransitionDeposits implements DepositManager with a no-op.
func (n *noopDepositManager) TransitionDeposits(context.Context,
	[]*deposit.Deposit, fsm.EventType, fsm.StateType) error {

	return nil
}

// LockDepositsForSwap implements DepositManager with a no-op.
func (n *noopDepositManager) LockDepositsForSwap(context.Context,
	[]*deposit.Deposit, lntypes.Hash) error {

	return nil
}

// ReleaseDepositsForSwap implements DepositManager with a no-op.
func (n *noopDepositManager) ReleaseDepositsForSwap(context.Context,
	[]*deposit.Deposit, lntypes.Hash) error {

	return nil
}

// DepositsForOutpoints implements DepositManager with a no-op.
func (n *noopDepositManager) DepositsForOutpoints(_ context.Context,
	outpoints []string, ignoreUnknown bool) ([]*deposit.Deposit, error) {

	if n.depositsForOutpoints != nil {
		n.depositsForOutpoints(outpoints, ignoreUnknown)
	}

	return n.deposits, n.depositErr
}

// GetActiveDepositsInState implements DepositManager with a no-op.
func (n *noopDepositManager) GetActiveDepositsInState(fsm.StateType) (
	[]*deposit.Deposit, error) {

	return nil, nil
}

type depositTransition struct {
	deposits []*deposit.Deposit
	event    fsm.EventType
	state    fsm.StateType
}

// recordingDepositManager records deposit transitions. Like the real deposit
// manager, it owns its deposit objects: a transition looks the deposits up by
// outpoint and updates the manager's own objects, not the ones passed in, and
// DepositsForOutpoints returns copies, like a store read.
type recordingDepositManager struct {
	noopDepositManager

	// active holds the manager's own deposit objects by outpoint.
	active map[wire.OutPoint]*deposit.Deposit

	err         error
	errs        []error
	transition  func([]*deposit.Deposit, fsm.EventType, fsm.StateType) error
	transitions []depositTransition

	transitionChan chan depositTransition
	events         []fsm.EventType
	states         []fsm.StateType

	// lockedFor and releasedFor record the swap hashes passed in.
	lockedFor   []lntypes.Hash
	releasedFor []lntypes.Hash
}

// newRecordingDepositManager returns a recordingDepositManager that owns the
// given deposit objects.
func newRecordingDepositManager(
	deposits ...*deposit.Deposit) *recordingDepositManager {

	active := make(map[wire.OutPoint]*deposit.Deposit, len(deposits))
	for _, d := range deposits {
		active[d.OutPoint] = d
	}

	return &recordingDepositManager{active: active}
}

// TransitionDeposits records the transition and returns the configured error.
// Like the real manager, it applies the transition to its own deposit objects
// with the outpoints of the given deposits.
func (r *recordingDepositManager) TransitionDeposits(_ context.Context,
	deposits []*deposit.Deposit, event fsm.EventType,
	state fsm.StateType) error {

	transition := depositTransition{
		deposits: deposits,
		event:    event,
		state:    state,
	}

	r.transitions = append(r.transitions, transition)
	r.events = append(r.events, event)
	r.states = append(r.states, state)

	if r.transitionChan != nil {
		r.transitionChan <- transition
	}

	activeDeposits := make([]*deposit.Deposit, 0, len(deposits))
	for i, d := range deposits {
		if d == nil {
			return fmt.Errorf("nil deposit at index %d", i)
		}

		activeDeposit, ok := r.active[d.OutPoint]
		if !ok {
			return fmt.Errorf("deposit %v not found in active "+
				"deposits", d.OutPoint)
		}

		activeDeposits = append(activeDeposits, activeDeposit)
	}

	switch {
	case r.transition != nil:
		err := r.transition(activeDeposits, event, state)
		if err != nil {
			return err
		}

	case len(r.errs) > 0:
		err := r.errs[0]
		r.errs = r.errs[1:]
		if err != nil {
			return err
		}

	case r.err != nil:
		return r.err
	}

	for _, d := range activeDeposits {
		d.SetState(state)
	}

	return nil
}

// LockDepositsForSwap records the swap hash and the transition to LoopingIn.
func (r *recordingDepositManager) LockDepositsForSwap(ctx context.Context,
	deposits []*deposit.Deposit, swapHash lntypes.Hash) error {

	r.lockedFor = append(r.lockedFor, swapHash)

	err := r.TransitionDeposits(
		ctx, deposits, deposit.OnLoopInInitiated, deposit.LoopingIn,
	)
	if err != nil {
		return err
	}

	for _, d := range deposits {
		if active, ok := r.active[d.OutPoint]; ok {
			hash := swapHash
			active.SwapHash = &hash
		}
	}

	return nil
}

// ReleaseDepositsForSwap records the swap hash and the transition to
// Deposited. Like the real manager, it leaves deposits recorded for another
// swap alone.
func (r *recordingDepositManager) ReleaseDepositsForSwap(ctx context.Context,
	deposits []*deposit.Deposit, swapHash lntypes.Hash) error {

	r.releasedFor = append(r.releasedFor, swapHash)

	owned := make([]*deposit.Deposit, 0, len(deposits))
	for _, d := range deposits {
		active, ok := r.active[d.OutPoint]
		if ok && active.SwapHash != nil &&
			*active.SwapHash != swapHash {

			continue
		}
		owned = append(owned, d)
	}
	if len(owned) == 0 {
		return nil
	}

	return r.TransitionDeposits(
		ctx, owned, fsm.OnError, deposit.Deposited,
	)
}

// DepositsForOutpoints returns copies of the manager's own deposit objects,
// like the real manager, which reads the deposits from the store.
func (r *recordingDepositManager) DepositsForOutpoints(_ context.Context,
	outpoints []string, ignoreUnknown bool) ([]*deposit.Deposit, error) {

	if r.depositsForOutpoints != nil {
		r.depositsForOutpoints(outpoints, ignoreUnknown)
	}

	deposits := make([]*deposit.Deposit, 0, len(outpoints))
	for _, outpoint := range outpoints {
		op, err := wire.NewOutPointFromString(outpoint)
		if err != nil {
			return nil, err
		}

		d, ok := r.active[*op]
		if !ok {
			if ignoreUnknown {
				continue
			}

			return nil, deposit.ErrDepositNotFound
		}

		storeCopy := &deposit.Deposit{
			OutPoint:           d.OutPoint,
			ID:                 d.ID,
			Value:              d.Value,
			ConfirmationHeight: d.GetConfirmationHeight(),
		}
		storeCopy.SetState(d.GetState())

		deposits = append(deposits, storeCopy)
	}

	return deposits, nil
}

type recordingRiskStore struct {
	*mockStore

	decisions chan ConfirmationRiskDecision
}

// RecordStaticAddressRiskDecision records a risk decision in the mock store.
func (s *recordingRiskStore) RecordStaticAddressRiskDecision(
	_ context.Context, swapHash lntypes.Hash,
	decision ConfirmationRiskDecision) error {

	loopIn, ok := s.loopIns[swapHash]
	if !ok {
		return ErrLoopInNotFound
	}

	loopIn.ConfirmationRiskDecision = decision
	loopIn.ConfirmationRiskDecisionTime = time.Now()

	select {
	case s.decisions <- decision:
	default:
	}

	return nil
}

// mockNotificationManager allows tests to push server notifications directly to
// monitor actions.
type mockNotificationManager struct {
	riskAccepted chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification
	riskRejected chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification
}

type silentBlockChainNotifier struct {
	lndclient.ChainNotifierClient
}

// RegisterBlockEpochNtfn implements ChainNotifierClient without delivering an
// initial block. Tests use it to assert current-height recovery behavior without
// relying on a block notification.
func (s *silentBlockChainNotifier) RegisterBlockEpochNtfn(context.Context) (
	chan int32, chan error, error) {

	return make(chan int32), make(chan error), nil
}

// SubscribeStaticLoopInSweepRequests implements NotificationManager.
func (m *mockNotificationManager) SubscribeStaticLoopInSweepRequests(
	context.Context) <-chan *swapserverrpc.ServerStaticLoopInSweepNotification {

	return make(chan *swapserverrpc.ServerStaticLoopInSweepNotification)
}

// SubscribeStaticLoopInRiskAccepted implements NotificationManager.
func (m *mockNotificationManager) SubscribeStaticLoopInRiskAccepted(
	context.Context, lntypes.Hash,
) <-chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification {

	return m.riskAccepted
}

// SubscribeStaticLoopInRiskRejected implements NotificationManager.
func (m *mockNotificationManager) SubscribeStaticLoopInRiskRejected(
	context.Context, lntypes.Hash,
) <-chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification {

	return m.riskRejected
}

type recordingTxOutChecker struct {
	outpoints [][]wire.OutPoint
	txOuts    map[wire.OutPoint]*wire.TxOut
	err       error

	// htlcSpent is what SpentByTxPayingTo returns.
	htlcSpent bool

	// htlcSpentResults, if set, are returned by SpentByTxPayingTo one
	// per call before it falls back to htlcSpent.
	htlcSpentResults []bool

	// spendOutpoints, spendPkScripts and spendStartHeights record what
	// SpentByTxPayingTo was asked about.
	spendOutpoints    [][]wire.OutPoint
	spendPkScripts    [][]byte
	spendStartHeights []int32
}

// GetTxOuts records the request and returns the configured available outputs.
func (r *recordingTxOutChecker) GetTxOuts(_ context.Context,
	outpoints []wire.OutPoint) (map[wire.OutPoint]*wire.TxOut, error) {

	r.outpoints = append(
		r.outpoints, append([]wire.OutPoint(nil), outpoints...),
	)
	if r.err != nil {
		return nil, r.err
	}

	return r.txOuts, nil
}

// SpentByTxPayingTo records the outpoints, pkScript and start height and
// returns the configured result.
func (r *recordingTxOutChecker) SpentByTxPayingTo(_ context.Context,
	outpoints []wire.OutPoint, pkScript []byte,
	startHeight int32) (bool, error) {

	r.spendOutpoints = append(
		r.spendOutpoints, append([]wire.OutPoint(nil), outpoints...),
	)
	r.spendPkScripts = append(r.spendPkScripts, pkScript)
	r.spendStartHeights = append(r.spendStartHeights, startHeight)
	if r.err != nil {
		return false, r.err
	}

	if len(r.htlcSpentResults) > 0 {
		spent := r.htlcSpentResults[0]
		r.htlcSpentResults = r.htlcSpentResults[1:]

		return spent, nil
	}

	return r.htlcSpent, nil
}

// initHtlcTestServer lets InitHtlcAction tests inject a deterministic server
// response without standing up the full gRPC client.
type initHtlcTestServer struct {
	swapserverrpc.StaticAddressServerClient

	loopInResp *swapserverrpc.ServerStaticAddressLoopInResponse
	loopInErr  error
	pushed     []*swapserverrpc.PushStaticAddressHtlcSigsRequest
}

// ServerStaticAddressLoopIn returns the canned response configured by the test.
func (s *initHtlcTestServer) ServerStaticAddressLoopIn(context.Context,
	*swapserverrpc.ServerStaticAddressLoopInRequest, ...grpc.CallOption,
) (*swapserverrpc.ServerStaticAddressLoopInResponse, error) {

	return s.loopInResp, s.loopInErr
}

// PushStaticAddressHtlcSigs records the abandonment signal in error-path tests.
func (s *initHtlcTestServer) PushStaticAddressHtlcSigs(_ context.Context,
	req *swapserverrpc.PushStaticAddressHtlcSigsRequest, _ ...grpc.CallOption,
) (*swapserverrpc.PushStaticAddressHtlcSigsResponse, error) {

	s.pushed = append(s.pushed, req)

	return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
}

// TestLegacyConfirmationFallbackKeepsLiveDeposits verifies that refreshing the
// confirmation heights for the legacy fallback reads fresh store copies but
// keeps the loop-in's own deposit objects. The deposit state machines update
// those objects; replacing them with copies made later state checks fail
// forever.
func TestLegacyConfirmationFallbackKeepsLiveDeposits(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{15},
		Index: 0,
	}
	live := &deposit.Deposit{OutPoint: outpoint}
	live.SetState(deposit.LoopingIn)
	storeCopy := &deposit.Deposit{
		OutPoint:           outpoint,
		ConfirmationHeight: 10,
	}
	storeCopy.SetState(deposit.LoopingIn)

	f := &FSM{
		cfg: &Config{
			DepositManager: &noopDepositManager{
				deposits: []*deposit.Deposit{storeCopy},
			},
		},
		loopIn: &StaticAddressLoopIn{
			DepositOutpoints: []string{outpoint.String()},
			Deposits:         []*deposit.Deposit{live},
		},
	}

	// The fresh copy is confirmed deeply enough.
	reached := f.shouldStartLegacyConfirmationFallback(
		t.Context(), int32(10+deposit.MinConfs),
	)
	require.True(t, reached)

	// The loop-in still refers to the live deposit object.
	require.Len(t, f.loopIn.Deposits, 1)
	require.Same(t, live, f.loopIn.Deposits[0])
}

// TestMonitorInvoiceAndHtlcTxSweepsTimeoutAfterLegacyRefresh verifies that an
// unpaid HTLC still moves to the timeout sweep after the legacy confirmation
// fallback reloaded the selected deposits. The deposit manager transitions its
// own deposit objects, so the monitor only sees the transition if the loop-in
// kept them instead of the reloaded copies.
func TestMonitorInvoiceAndHtlcTxSweepsTimeoutAfterLegacyRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{29, 30, 31}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		mockLnd.LndServices.Invoices,
	)
	f.loopIn.HtlcCltvExpiry = mockLnd.Height
	f.loopIn.PaymentTimeoutSeconds = 1

	// The selected deposit is confirmed deeply enough for the legacy
	// fallback, which reloads it and starts the payment deadline.
	liveDeposit := f.loopIn.Deposits[0]
	liveDeposit.SetState(deposit.LoopingIn)
	confirmationHeight := int64(mockLnd.Height) - deposit.MinConfs + 1
	liveDeposit.ConfirmationHeight = confirmationHeight
	f.loopIn.DepositOutpoints = []string{liveDeposit.OutPoint.String()}

	htlcConf := htlcConfirmation(t, f)

	reloaded := make(chan struct{}, 1)
	depositMgr.depositsForOutpoints = func([]string, bool) {
		select {
		case reloaded <- struct{}{}:

		default:
		}
	}

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:

	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	var confRegistration *test.ConfRegistration
	select {
	case confRegistration = <-mockLnd.RegisterConfChannel:

	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	select {
	case <-reloaded:

	case <-ctx.Done():
		t.Fatalf("deposits were not reloaded: %v", ctx.Err())
	}

	// The HTLC confirms and the payment deadline passes without payment,
	// which locks the deposits for the timeout sweep.
	confRegistration.ConfChan <- htlcConf

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposits were not locked for timeout sweeping: %v",
			ctx.Err())
	}

	// Once the HTLC times out, the swap sweeps it back.
	require.NoError(t, mockLnd.NotifyHeight(mockLnd.Height+1))

	select {
	case event := <-resultChan:
		require.Equal(t, OnSweepHtlcTimeout, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not exit: %v", ctx.Err())
	}

	require.Same(t, liveDeposit, f.loopIn.Deposits[0])
	require.True(t, liveDeposit.IsInState(deposit.SweepHtlcTimeout))
}

// TestInitHtlcActionValidatesSwapFee verifies that invalid quotes cannot
// create invoices and that valid fees retain the exact invoice amount.
func TestInitHtlcActionValidatesSwapFee(t *testing.T) {
	t.Parallel()

	const depositAmount = btcutil.Amount(10_000)
	tests := []struct {
		name           string
		fee            btcutil.Amount
		selectedAmount btcutil.Amount
		valid          bool
	}{
		{name: "negative", fee: -1},
		{name: "minimum integer", fee: math.MinInt64},
		{name: "wrapped one sat invoice", fee: math.MinInt64 + 9_999},
		{name: "equal to amount", fee: depositAmount},
		{name: "above amount", fee: depositAmount + 1},
		{name: "maximum integer", fee: math.MaxInt64},
		{
			name:           "equal to partial swap amount",
			fee:            5_000,
			selectedAmount: 5_000,
		},
		{name: "zero fee", fee: 0, valid: true},
		{name: "positive fee", fee: 1_000, valid: true},
		{
			name:  "one sat remaining",
			fee:   depositAmount - 1,
			valid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockLnd := test.NewMockLnd()
			_, serverKey := test.CreateKey(21)
			server := &mockStaticAddressServer{
				response: testStaticAddressLoopInResponse(
					serverKey.SerializeCompressed(),
				),
			}
			dep := &deposit.Deposit{Value: depositAmount}
			manager := newRecordingDepositManager(dep)
			loopIn := &StaticAddressLoopIn{
				Deposits: []*deposit.Deposit{dep},
				DepositOutpoints: []string{
					dep.OutPoint.String(),
				},
				SelectedAmount:   tc.selectedAmount,
				QuotedSwapFee:    tc.fee,
				InitiationHeight: uint32(mockLnd.Height),
				InitiationTime:   time.Now(),
			}
			f := &FSM{
				StateMachine: &fsm.StateMachine{},
				cfg: &Config{
					Server:         server,
					DepositManager: manager,
					LndClient:      mockLnd.Client,
					WalletKit:      mockLnd.WalletKit,
					ChainParams:    mockLnd.ChainParams,
					Store:          &mockStore{},

					MaxStaticAddrHtlcFeePercentage:       1,
					MaxStaticAddrHtlcBackupFeePercentage: 1,
				},
				loopIn: loopIn,
			}

			f.cfg.ValidateLoopInContract = testValidateLoopInContract

			event := f.InitHtlcAction(t.Context(), nil)
			if !tc.valid {
				require.Equal(t, fsm.OnError, event)
				require.ErrorIs(
					t, f.LastActionError, ErrInvalidSwapFee,
				)
				require.Empty(t, mockLnd.Invoices)
				require.Empty(t, loopIn.SwapInvoice)
				require.Nil(t, server.request)

				// The init error path releases the deposits.
				states := f.LoopInStatesV0()
				require.Equal(t, UnlockDeposits,
					states[InitHtlcTx].Transitions[event])
				states[UnlockDeposits].Action(t.Context(), nil)
				require.Equal(
					t, deposit.Deposited, dep.GetState(),
				)
				return
			}

			require.Equal(t, OnHtlcInitiated, event)
			require.NoError(t, f.LastActionError)
			require.NotNil(t, server.request)
			invoice, err := zpay32.Decode(
				server.request.SwapInvoice, mockLnd.ChainParams,
			)
			require.NoError(t, err)
			require.NotNil(t, invoice.MilliSat)
			require.Equal(t, lnwire.NewMSatFromSatoshis(
				depositAmount-tc.fee,
			), *invoice.MilliSat)
		})
	}
}

// TestInitHtlcActionRejectsMissingSigningInfo verifies that incomplete responses
// abort the swap, cancel its invoice and release deposits instead of panicking.
func TestInitHtlcActionRejectsMissingSigningInfo(t *testing.T) {
	t.Parallel()

	for _, missing := range []string{
		"standard", "high fee", "extreme fee", "all", "response",
	} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()

			mockLnd := test.NewMockLnd()
			_, serverKey := test.CreateKey(21)
			response := testStaticAddressLoopInResponse(
				serverKey.SerializeCompressed(),
			)
			switch missing {
			case "standard":
				response.StandardHtlcInfo = nil
			case "high fee":
				response.HighFeeHtlcInfo = nil
			case "extreme fee":
				response.ExtremeFeeHtlcInfo = nil
			case "all":
				response.StandardHtlcInfo = nil
				response.HighFeeHtlcInfo = nil
				response.ExtremeFeeHtlcInfo = nil
			case "response":
				response = nil
			}
			server := &initHtlcTestServer{loopInResp: response}
			dep := &deposit.Deposit{Value: 500_000}
			manager := newRecordingDepositManager(dep)
			loopIn := &StaticAddressLoopIn{
				Deposits: []*deposit.Deposit{dep},
				DepositOutpoints: []string{
					dep.OutPoint.String(),
				},
				QuotedSwapFee:    1_000,
				InitiationHeight: uint32(mockLnd.Height),
			}
			f := &FSM{
				StateMachine: &fsm.StateMachine{},
				loopIn:       loopIn,
				cfg: &Config{
					Server:         server,
					DepositManager: manager,
					LndClient:      mockLnd.Client,
					WalletKit:      mockLnd.WalletKit,
					ChainParams:    mockLnd.ChainParams,
				},
			}
			f.cfg.InvoicesClient = mockLnd.LndServices.Invoices
			f.cfg.ValidateLoopInContract = testValidateLoopInContract

			var event fsm.EventType
			require.NotPanics(t, func() {
				event = f.InitHtlcAction(t.Context(), nil)
			})
			require.Equal(t, fsm.OnError, event)
			require.EqualError(t, f.LastActionError,
				"server response is missing htlc signing info")

			// Only an empty abandonment request is sent to the server.
			require.Len(t, server.pushed, 1)
			require.Equal(t,
				&swapserverrpc.PushStaticAddressHtlcSigsRequest{
					SwapHash: loopIn.SwapHash[:],
				}, server.pushed[0])

			// Invoice cancellation happens before the action returns.
			require.NotEmpty(t, loopIn.SwapInvoice)
			select {
			case hash := <-mockLnd.FailInvoiceChannel:
				require.Equal(t, loopIn.SwapHash, hash)
			default:
				t.Fatal("swap invoice was not canceled")
			}

			states := f.LoopInStatesV0()
			require.Equal(t, UnlockDeposits,
				states[InitHtlcTx].Transitions[event])
			states[UnlockDeposits].Action(t.Context(), nil)
			require.Equal(t, deposit.Deposited, dep.GetState())
		})
	}
}
