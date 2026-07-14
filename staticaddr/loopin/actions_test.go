package loopin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const testTimeout = 5 * time.Second

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

	depositMgr := &recordingDepositManager{
		transitionChan: make(chan depositTransition, 1),
	}
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
	_, clientPubkey := test.CreateKey(20)
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
		AddressParams: &address.Parameters{
			ClientPubkey: clientPubkey,
		},
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
	require.EqualValues(
		t, swap.StaticAddressKeyFamily, loopIn.HtlcKeyLocator.Family,
	)
	require.Equal(
		t, clientPubkey.SerializeCompressed(),
		server.request.DepositToClientPubkeys[dep.String()],
	)

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

// TestInitHtlcActionSendsChangeOutput asserts that fractional loop-ins create
// and send an operation-specific static change output to the server.
func TestInitHtlcActionSendsChangeOutput(t *testing.T) {
	t.Parallel()

	mockLnd := test.NewMockLnd()
	_, depositClientPubkey := test.CreateKey(31)
	_, changeClientPubkey := test.CreateKey(32)
	_, serverKey := test.CreateKey(33)

	server := &mockStaticAddressServer{
		response: testStaticAddressLoopInResponse(
			serverKey.SerializeCompressed(),
		),
	}

	dep := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 0,
		},
		Value: 500_000,
		AddressParams: &address.Parameters{
			ClientPubkey: depositClientPubkey,
		},
	}
	changeParams := &address.Parameters{
		ID:           1,
		ClientPubkey: changeClientPubkey,
		PkScript:     []byte{0x51, 0x20, 0x01},
	}

	loopIn := &StaticAddressLoopIn{
		Deposits:              []*deposit.Deposit{dep},
		DepositOutpoints:      []string{dep.OutPoint.String()},
		SelectedAmount:        300_000,
		QuotedSwapFee:         1_000,
		InitiationHeight:      uint32(mockLnd.Height),
		InitiationTime:        time.Now(),
		PaymentTimeoutSeconds: 3_600,
	}

	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			Server:                               server,
			AddressManager:                       &mockAddressManager{params: changeParams},
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
	require.NotNil(t, server.request.ChangeOutput)
	require.EqualValues(t, 200_000, server.request.ChangeOutput.Amount)
	require.Equal(
		t, changeClientPubkey.SerializeCompressed(),
		server.request.ChangeOutput.ClientPubkey,
	)
	require.Equal(t, changeParams.PkScript, server.request.ChangeOutput.PkScript)
	require.Same(t, changeParams, loopIn.ChangeAddressParams)
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

type recordingLoopInStore struct {
	mockStore

	updates []*StaticAddressLoopIn
}

func (s *recordingLoopInStore) UpdateLoopIn(_ context.Context,
	loopIn *StaticAddressLoopIn) error {

	s.updates = append(s.updates, loopIn)

	return nil
}

// TestRecordConfirmedHtlcPersistsOutpoint verifies that the FSM records the
// exact confirmed server HTLC output before the timeout branch can sweep it.
func TestRecordConfirmedHtlcPersistsOutpoint(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		SwapHash:       lntypes.Hash{1, 2, 4},
		HtlcCltvExpiry: 800,
		ClientPubkey:   clientKey.PubKey(),
		ServerPubkey:   serverKey.PubKey(),
	}
	htlc, err := loopIn.getHtlc(test.NewMockLnd().ChainParams)
	require.NoError(t, err)

	htlcValue := int64(123_456)
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{
		Value:    1,
		PkScript: []byte{0x51},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    htlcValue,
		PkScript: htlc.PkScript,
	})

	store := &recordingLoopInStore{}
	f := &FSM{
		cfg:    &Config{Store: store},
		loopIn: loopIn,
	}

	err = f.recordConfirmedHtlc(
		t.Context(), &chainntnfs.TxConfirmation{Tx: tx},
		htlc.PkScript,
	)
	require.NoError(t, err)

	txHash := tx.TxHash()
	require.NotNil(t, loopIn.HtlcTxHash)
	require.Equal(t, txHash, *loopIn.HtlcTxHash)
	require.EqualValues(t, 1, loopIn.HtlcOutputIndex)
	require.EqualValues(t, htlcValue, loopIn.HtlcOutputValue)
	require.Len(t, store.updates, 1)
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
	depositMgr := &recordingDepositManager{
		transitionChan: make(chan depositTransition, 1),
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
		DepositManager:      depositMgr,
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: notificationMgr,
		Store:               &recordingLoopInStore{},
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)
	htlc, err := loopIn.getHtlc(mockLnd.ChainParams)
	require.NoError(t, err)
	htlcTx := wire.NewMsgTx(2)
	htlcTx.AddTxOut(&wire.TxOut{
		Value:    1,
		PkScript: htlc.PkScript,
	})

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
	confRegistration.ConfChan <- &chainntnfs.TxConfirmation{Tx: htlcTx}

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

	depositMgr := &recordingDepositManager{
		transitionChan: make(chan depositTransition, 1),
	}

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

	depositMgr := &recordingDepositManager{
		transitionChan: make(chan depositTransition, 1),
	}

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

	depositMgr := &recordingDepositManager{
		transitionChan: make(chan depositTransition, 1),
	}
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
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		Deposits: []*deposit.Deposit{{
			Value: 200_000,
			AddressParams: &address.Parameters{
				ClientPubkey: clientKey.PubKey(),
			},
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

// TestInitHtlcActionCancelsInvoiceOnFeeGuardFailure verifies that the early
// fee guard also cancels the pre-created invoice before returning an error.
func TestInitHtlcActionCancelsInvoiceOnFeeGuardFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		Deposits: []*deposit.Deposit{{
			Value: 200_000,
			AddressParams: &address.Parameters{
				ClientPubkey: clientKey.PubKey(),
			},
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
	depositMgr := &recordingDepositManager{}

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
	depositMgr := &recordingDepositManager{
		err: errors.New("transition failed"),
	}
	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			DepositManager: depositMgr,
		},
		loopIn: &StaticAddressLoopIn{
			Deposits: []*deposit.Deposit{{Value: 200_000}},
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
	params *script.Parameters
}

// GetStaticAddressParameters returns the configured address parameters.
func (m *mockAddressManager) GetStaticAddressParameters(_ context.Context) (
	*script.Parameters, error) {

	return m.params, nil
}

// GetStaticAddress is unused for this test and returns nil.
func (m *mockAddressManager) GetStaticAddress(_ context.Context) (
	*script.StaticAddress, error) {

	return nil, nil
}

// NewChangeAddress returns configured parameters for tests that need change.
func (m *mockAddressManager) NewChangeAddress(_ context.Context) (
	*address.Parameters, error) {

	return m.params, nil
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

type recordingDepositManager struct {
	noopDepositManager

	err         error
	errs        []error
	transitions []depositTransition

	transitionChan chan depositTransition
	events         []fsm.EventType
	states         []fsm.StateType
}

// TransitionDeposits records the transition and returns the configured error.
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

	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]

		return err
	}

	return r.err
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

// initHtlcTestServer lets InitHtlcAction tests inject a deterministic server
// response without standing up the full gRPC client.
type initHtlcTestServer struct {
	swapserverrpc.StaticAddressServerClient

	loopInResp *swapserverrpc.ServerStaticAddressLoopInResponse
	loopInErr  error
}

// ServerStaticAddressLoopIn returns the canned response configured by the test.
func (s *initHtlcTestServer) ServerStaticAddressLoopIn(context.Context,
	*swapserverrpc.ServerStaticAddressLoopInRequest, ...grpc.CallOption,
) (*swapserverrpc.ServerStaticAddressLoopInResponse, error) {

	return s.loopInResp, s.loopInErr
}

// PushStaticAddressHtlcSigs accepts the abandonment signal used by error-path
// tests without adding additional assertions.
func (s *initHtlcTestServer) PushStaticAddressHtlcSigs(context.Context,
	*swapserverrpc.PushStaticAddressHtlcSigsRequest, ...grpc.CallOption,
) (*swapserverrpc.PushStaticAddressHtlcSigsResponse, error) {

	return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
}
