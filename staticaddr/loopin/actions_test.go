package loopin

import (
	"context"
	"errors"
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
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

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
			params: &address.Parameters{
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

// TestInitHtlcActionPreservesRouteHints asserts that static-address loop-in
// propagates explicit route hints into the encoded swap invoice sent to the
// server. This currently fails because lndclient.AddInvoice drops route hints.
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

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &address.Parameters{
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

	cancel()
	select {
	case event := <-resultChan:
		require.Equal(t, fsm.OnError, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxStartsDeadlineAtLegacyMinConfs verifies that the
// monitor action preserves the legacy payment deadline fallback when no risk
// notification manager is available.
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
			params: &address.Parameters{
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
		require.Equal(t, fsm.OnError, event)

	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxStartsLegacyFallbackWithNotificationManager
// verifies that old servers that do not send risk notifications still get the
// legacy payment deadline even when the notification manager is configured.
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
			params: &address.Parameters{
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
		require.Equal(t, fsm.OnError, event)

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
			params: &address.Parameters{
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
			params: &address.Parameters{
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
			params: &address.Parameters{
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
			params: &address.Parameters{
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
			params: &address.Parameters{
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

// mockAddressManager is a minimal AddressManager implementation used by the
// test FSM setup.
type mockAddressManager struct {
	params *address.Parameters
}

// GetStaticAddressParameters returns the configured address parameters.
func (m *mockAddressManager) GetStaticAddressParameters(_ context.Context) (
	*address.Parameters, error) {

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

type recordingDepositManager struct {
	noopDepositManager

	events []fsm.EventType
	states []fsm.StateType
}

// TransitionDeposits records transition requests.
func (r *recordingDepositManager) TransitionDeposits(_ context.Context,
	_ []*deposit.Deposit, event fsm.EventType,
	expectedFinalState fsm.StateType) error {

	r.events = append(r.events, event)
	r.states = append(r.states, expectedFinalState)

	return nil
}

// mockNotificationManager allows tests to push server notifications directly to
// monitor actions.
type mockNotificationManager struct {
	riskAccepted chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification
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

type testTxOutChecker struct {
	txOut *wire.TxOut
	err   error

	outpoints      []wire.OutPoint
	includeMempool []bool
}

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
