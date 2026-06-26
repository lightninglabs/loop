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
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

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
			name:      "unexpected",
			state:     invoices.ContractState(99),
			event:     fsm.OnError,
			done:      true,
			errString: "unexpected invoice state",
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

// TestMonitorInvoiceAndHtlcTxReRegistersOnConfErr ensures that an error from
// the HTLC confirmation subscription triggers a re-registration. Without the
// regression fix, only the initial registration would be performed and the
// test would time out waiting for the second one.
func TestMonitorInvoiceAndHtlcTxReRegistersOnConfErr(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
		InitiationTime:        time.Now(),
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	case <-time.After(5 * time.Second):
		t.Fatal("timeout sweep monitor did not return")
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
	checker := &testTxOutChecker{}

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
	require.Equal(t, []wire.OutPoint{dep.OutPoint}, checker.outpoints)
	require.Equal(t, []bool{true}, checker.includeMempool)
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

// TestMonitorInvoiceAndHtlcTxStartsDeadlineOnRiskAccepted verifies that the
// payment timeout does not start until the server notifies us that confirmation
// risk was accepted.
func TestMonitorInvoiceAndHtlcTxStartsDeadlineOnRiskAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

// TestMonitorInvoiceAndHtlcTxCancelsOnRiskRejected verifies that a server-side
// confirmation risk rejection is terminal for the client monitor action.
func TestMonitorInvoiceAndHtlcTxCancelsOnRiskRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
		riskRejected: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification, 1,
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	txOutChecker := &testTxOutChecker{}
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
	require.Empty(t, txOutChecker.includeMempool)
}

// TestMonitorInvoiceAndHtlcTxDoesNotCancelAcceptedInvoiceForMissingOutpoint
// verifies that the outpoint-vanished fallback is only active before payment
// has started. Once the invoice is accepted, the original deposit may disappear
// because the server has moved forward with the swap.
func TestMonitorInvoiceAndHtlcTxDoesNotCancelAcceptedInvoiceForMissingOutpoint(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
		TxOutChecker:   &testTxOutChecker{},
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

// TestMonitorInvoiceAndHtlcTxUnlocksOnHtlcTimeoutWithoutDeadline verifies that
// deposits are unlocked even if the payment deadline never started before the
// HTLC timeout path opened.
func TestMonitorInvoiceAndHtlcTxUnlocksOnHtlcTimeoutWithoutDeadline(
	t *testing.T) {

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

// TestOriginalDepositOutpointUnavailableRequiresMissingTxOut verifies that a
// present txout does not trigger the RBF cancellation path.
func TestOriginalDepositOutpointUnavailableRequiresMissingTxOut(t *testing.T) {
	originalOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 0,
	}

	txOutChecker := &testTxOutChecker{
		txOut: &wire.TxOut{Value: 10_000},
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
	require.Equal(t, []wire.OutPoint{originalOutpoint}, txOutChecker.outpoints)
	require.Equal(t, []bool{true}, txOutChecker.includeMempool)
}

// TestSignHtlcTxActionCancelsWhenOriginalOutpointUnavailable verifies that a
// pending loop-in is canceled before HTLC signing if GetTxOut with mempool
// awareness reports that one of the originally selected outpoints is gone.
func TestSignHtlcTxActionCancelsWhenOriginalOutpointUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	txOutChecker := &testTxOutChecker{}
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

	require.Equal(t, []wire.OutPoint{originalOutpoint}, txOutChecker.outpoints)
	require.Equal(t, []bool{true}, txOutChecker.includeMempool)
}

// TestSignHtlcTxActionDoesNotCancelOnTxOutLookupError verifies that lookup
// failures are treated as errors, but do not cancel the invoice. The invoice is
// only canceled when GetTxOut explicitly returns nil for an original outpoint.
func TestSignHtlcTxActionDoesNotCancelOnTxOutLookupError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	txOutChecker := &testTxOutChecker{
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

// TestInitHtlcActionCancelsInvoiceOnFeeGuardFailure verifies that the early
// fee guard also cancels the pre-created invoice before returning an error.
func TestInitHtlcActionCancelsInvoiceOnFeeGuardFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

// noopDepositManager is a stub DepositManager used to satisfy FSM config.
type noopDepositManager struct {
	deposits             []*deposit.Deposit
	depositsForOutpoints func([]string, bool)
	depositErr           error
}

// EnsureDepositsFresh implements DepositManager with a no-op.
func (n *noopDepositManager) EnsureDepositsFresh(context.Context) error {
	return nil
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

	err            error
	transitions    []depositTransition
	transitionChan chan depositTransition

	events []fsm.EventType
	states []fsm.StateType
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

	return r.err
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

type testTxOutChecker struct {
	txOut *wire.TxOut
	err   error

	outpoints      []wire.OutPoint
	includeMempool []bool
}

// GetTxOut records lookup parameters and returns the configured result.
func (t *testTxOutChecker) GetTxOut(_ context.Context,
	outpoint wire.OutPoint, includeMempool bool) (*wire.TxOut, error) {

	t.outpoints = append(t.outpoints, outpoint)
	t.includeMempool = append(t.includeMempool, includeMempool)

	return t.txOut, t.err
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
