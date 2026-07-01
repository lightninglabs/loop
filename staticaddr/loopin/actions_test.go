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
	mockLnd.Invoices[swapHash] = &lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
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
type noopDepositManager struct{}

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
func (n *noopDepositManager) DepositsForOutpoints(context.Context, []string,
	bool) ([]*deposit.Deposit, error) {

	return nil, nil
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
	transitions []depositTransition
}

// TransitionDeposits records the transition and returns the configured error.
func (r *recordingDepositManager) TransitionDeposits(_ context.Context,
	deposits []*deposit.Deposit, event fsm.EventType,
	state fsm.StateType) error {

	r.transitions = append(r.transitions, depositTransition{
		deposits: deposits,
		event:    event,
		state:    state,
	})

	return r.err
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
