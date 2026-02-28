package openchannel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type transitionCall struct {
	event         fsm.EventType
	expectedState fsm.StateType
	outpoints     []wire.OutPoint
}

type mockDepositManager struct {
	openingDeposits []*deposit.Deposit
	getErr          error
	transitionErrs  map[fsm.EventType]error
	calls           []transitionCall
}

func (m *mockDepositManager) AllOutpointsActiveDeposits([]wire.OutPoint,
	fsm.StateType) ([]*deposit.Deposit, bool) {

	return nil, false
}

func (m *mockDepositManager) GetActiveDepositsInState(stateFilter fsm.StateType) (
	[]*deposit.Deposit, error) {

	if stateFilter != deposit.OpeningChannel {
		return nil, nil
	}

	if m.getErr != nil {
		return nil, m.getErr
	}

	return m.openingDeposits, nil
}

func (m *mockDepositManager) TransitionDeposits(_ context.Context,
	deposits []*deposit.Deposit, event fsm.EventType,
	expectedFinalState fsm.StateType) error {

	call := transitionCall{
		event:         event,
		expectedState: expectedFinalState,
		outpoints:     make([]wire.OutPoint, len(deposits)),
	}
	for i, d := range deposits {
		call.outpoints[i] = d.OutPoint
	}
	m.calls = append(m.calls, call)

	if err, ok := m.transitionErrs[event]; ok {
		return err
	}

	return nil
}

type mockWalletKit struct {
	lndclient.WalletKitClient

	utxos []*lnwallet.Utxo
	err   error
	calls int
}

func (m *mockWalletKit) ListUnspent(_ context.Context, _, _ int32,
	_ ...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	m.calls++

	if m.err != nil {
		return nil, m.err
	}

	return m.utxos, nil
}

// TestRecoverOpeningChannelDepositsMixed verifies that recovery correctly
// classifies deposits based on UTXO status: unspent deposits are moved back to
// Deposited and spent deposits are transitioned to ChannelPublished.
func TestRecoverOpeningChannelDepositsMixed(t *testing.T) {
	t.Parallel()

	unspentDeposit := &deposit.Deposit{OutPoint: testOutPoint(1)}
	spentDeposit := &deposit.Deposit{OutPoint: testOutPoint(2)}

	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{unspentDeposit, spentDeposit},
	}
	walletKit := &mockWalletKit{
		utxos: []*lnwallet.Utxo{
			{OutPoint: unspentDeposit.OutPoint},
			{OutPoint: testOutPoint(99)},
		},
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, walletKit.calls)
	require.Len(t, depositManager.calls, 2)

	require.Equal(t, fsm.OnError, depositManager.calls[0].event)
	require.Equal(t, deposit.Deposited, depositManager.calls[0].expectedState)
	require.Equal(
		t, []wire.OutPoint{unspentDeposit.OutPoint},
		depositManager.calls[0].outpoints,
	)

	require.Equal(t, deposit.OnChannelPublished, depositManager.calls[1].event)
	require.Equal(
		t, deposit.ChannelPublished,
		depositManager.calls[1].expectedState,
	)
	require.Equal(
		t, []wire.OutPoint{spentDeposit.OutPoint},
		depositManager.calls[1].outpoints,
	)
}

// TestRecoverOpeningChannelDepositsNoDeposits verifies that recovery is a
// no-op when there are no deposits in the OpeningChannel state.
func TestRecoverOpeningChannelDepositsNoDeposits(t *testing.T) {
	t.Parallel()

	depositManager := &mockDepositManager{}
	walletKit := &mockWalletKit{}
	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.NoError(t, err)
	require.Zero(t, walletKit.calls)
	require.Empty(t, depositManager.calls)
}

// TestRecoverOpeningChannelDepositsListUnspentError verifies that a
// ListUnspent failure during recovery is propagated to the caller.
func TestRecoverOpeningChannelDepositsListUnspentError(t *testing.T) {
	t.Parallel()

	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{
			{OutPoint: testOutPoint(1)},
		},
	}
	walletKit := &mockWalletKit{
		err: errors.New("list unspent failed"),
	}
	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.ErrorContains(t, err, "unable to list unspent outputs")
	require.Empty(t, depositManager.calls)
}

// TestRecoverOpeningChannelDepositsTransitionError verifies that a transition
// failure when moving unspent deposits back to Deposited is propagated.
func TestRecoverOpeningChannelDepositsTransitionError(t *testing.T) {
	t.Parallel()

	unspentDeposit := &deposit.Deposit{OutPoint: testOutPoint(1)}
	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{unspentDeposit},
		transitionErrs: map[fsm.EventType]error{
			fsm.OnError: errors.New("transition failed"),
		},
	}
	walletKit := &mockWalletKit{
		utxos: []*lnwallet.Utxo{
			{OutPoint: unspentDeposit.OutPoint},
		},
	}
	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.ErrorContains(t, err, "unable to recover unspent opening deposits")
	require.Len(t, depositManager.calls, 1)
}

// TestRecoverAfterReorg simulates a reorg where a channel funding transaction
// was confirmed but then reorged out. After the reorg the deposit UTXOs
// reappear as unspent, so recovery should move all deposits back to Deposited.
func TestRecoverAfterReorg(t *testing.T) {
	t.Parallel()

	d1 := &deposit.Deposit{OutPoint: testOutPoint(1)}
	d2 := &deposit.Deposit{OutPoint: testOutPoint(2)}

	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{d1, d2},
	}
	walletKit := &mockWalletKit{
		utxos: []*lnwallet.Utxo{
			// After reorg both UTXOs are unspent again.
			{OutPoint: d1.OutPoint},
			{OutPoint: d2.OutPoint},
		},
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.NoError(t, err)
	require.Len(t, depositManager.calls, 1)

	// Both deposits should transition back to Deposited.
	require.Equal(t, fsm.OnError, depositManager.calls[0].event)
	require.Equal(
		t, deposit.Deposited, depositManager.calls[0].expectedState,
	)
	require.ElementsMatch(
		t,
		[]wire.OutPoint{d1.OutPoint, d2.OutPoint},
		depositManager.calls[0].outpoints,
	)
}

// TestRecoverAfterMempoolEviction simulates the case where the channel funding
// transaction was evicted from the mempool. The deposit UTXOs reappear as
// unspent, so recovery should move them back to Deposited.
func TestRecoverAfterMempoolEviction(t *testing.T) {
	t.Parallel()

	d := &deposit.Deposit{OutPoint: testOutPoint(1)}
	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{d},
	}
	walletKit := &mockWalletKit{
		utxos: []*lnwallet.Utxo{
			// UTXO reappears after mempool eviction.
			{OutPoint: d.OutPoint},
		},
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.NoError(t, err)
	require.Len(t, depositManager.calls, 1)
	require.Equal(t, fsm.OnError, depositManager.calls[0].event)
	require.Equal(
		t, deposit.Deposited, depositManager.calls[0].expectedState,
	)
}

// TestRecoverAfterMempoolRejection simulates the case where the channel
// funding transaction was rejected from the mempool (e.g. fee too low). The
// deposit UTXOs were never spent, so recovery should move them back to
// Deposited.
func TestRecoverAfterMempoolRejection(t *testing.T) {
	t.Parallel()

	d1 := &deposit.Deposit{OutPoint: testOutPoint(1)}
	d2 := &deposit.Deposit{OutPoint: testOutPoint(2)}
	d3 := &deposit.Deposit{OutPoint: testOutPoint(3)}

	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{d1, d2, d3},
	}
	walletKit := &mockWalletKit{
		utxos: []*lnwallet.Utxo{
			// All UTXOs still unspent since tx was never accepted.
			{OutPoint: d1.OutPoint},
			{OutPoint: d2.OutPoint},
			{OutPoint: d3.OutPoint},
		},
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.NoError(t, err)
	require.Len(t, depositManager.calls, 1)
	require.Equal(t, fsm.OnError, depositManager.calls[0].event)
	require.Equal(
		t, deposit.Deposited, depositManager.calls[0].expectedState,
	)
	require.Len(t, depositManager.calls[0].outpoints, 3)
}

// TestRecoverDaemonRestartChannelPublished simulates a daemon restart where
// the channel funding tx was successfully broadcast and the deposit UTXOs are
// all spent. Recovery should move them to ChannelPublished.
func TestRecoverDaemonRestartChannelPublished(t *testing.T) {
	t.Parallel()

	d1 := &deposit.Deposit{OutPoint: testOutPoint(1)}
	d2 := &deposit.Deposit{OutPoint: testOutPoint(2)}

	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{d1, d2},
	}
	walletKit := &mockWalletKit{
		// No UTXOs returned - all deposit outpoints have been spent.
		utxos: []*lnwallet.Utxo{},
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.NoError(t, err)
	require.Len(t, depositManager.calls, 1)
	require.Equal(
		t, deposit.OnChannelPublished,
		depositManager.calls[0].event,
	)
	require.Equal(
		t, deposit.ChannelPublished,
		depositManager.calls[0].expectedState,
	)
	require.ElementsMatch(
		t,
		[]wire.OutPoint{d1.OutPoint, d2.OutPoint},
		depositManager.calls[0].outpoints,
	)
}

// TestRecoverChannelPublishedTransitionError verifies that an error
// transitioning deposits to ChannelPublished during recovery is returned.
func TestRecoverChannelPublishedTransitionError(t *testing.T) {
	t.Parallel()

	d := &deposit.Deposit{OutPoint: testOutPoint(1)}
	depositManager := &mockDepositManager{
		openingDeposits: []*deposit.Deposit{d},
		transitionErrs: map[fsm.EventType]error{
			deposit.OnChannelPublished: errors.New(
				"transition failed",
			),
		},
	}
	walletKit := &mockWalletKit{
		// UTXO is spent, so recovery tries ChannelPublished transition.
		utxos: []*lnwallet.Utxo{},
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
			WalletKit:      walletKit,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.ErrorContains(t, err, "unable to recover spent opening deposits")
}

// TestRecoverGetActiveDepositsError verifies that a failure to fetch opening
// channel deposits is surfaced.
func TestRecoverGetActiveDepositsError(t *testing.T) {
	t.Parallel()

	depositManager := &mockDepositManager{
		getErr: errors.New("db connection lost"),
	}

	manager := &Manager{
		cfg: &Config{
			DepositManager: depositManager,
		},
	}

	err := manager.recoverOpeningChannelDeposits(context.Background())
	require.ErrorContains(t, err, "unable to fetch opening channel deposits")
}

func testOutPoint(b byte) wire.OutPoint {
	return wire.OutPoint{
		Hash:  chainhash.Hash{b},
		Index: uint32(b),
	}
}

// TestOpenChannelDuplicateOutpoints verifies that OpenChannel rejects requests
// containing duplicate outpoints, which would cause fee miscalculation and an
// invalid PSBT with the same input listed twice.
func TestOpenChannelDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	op := testOutPoint(1)
	manager := &Manager{
		cfg: &Config{},
	}

	req := &lnrpc.OpenChannelRequest{
		NodePubkey:         make([]byte, 33),
		LocalFundingAmount: 100000,
		SatPerVbyte:        10,
		Outpoints: []*lnrpc.OutPoint{
			{
				TxidStr:     op.Hash.String(),
				OutputIndex: op.Index,
			},
			{
				TxidStr:     op.Hash.String(),
				OutputIndex: op.Index,
			},
		},
	}

	_, err := manager.OpenChannel(context.Background(), req)
	require.ErrorContains(t, err, "duplicate outpoint")
}

// TestValidateInitialPsbtFlags verifies that request fields incompatible with
// PSBT funding are rejected early, before any deposits are locked.
func TestValidateInitialPsbtFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		req               *lnrpc.OpenChannelRequest
		expectedErrSubstr string
	}{
		{
			name: "default min confs accepted",
			req:  &lnrpc.OpenChannelRequest{},
		},
		{
			name: "explicit default min confs accepted",
			req: &lnrpc.OpenChannelRequest{
				MinConfs: defaultUtxoMinConf,
			},
		},
		{
			name: "custom min confs rejected",
			req: &lnrpc.OpenChannelRequest{
				MinConfs: defaultUtxoMinConf + 1,
			},
			expectedErrSubstr: "custom MinConfs not supported",
		},
		{
			name: "spend unconfirmed rejected",
			req: &lnrpc.OpenChannelRequest{
				MinConfs:         defaultUtxoMinConf,
				SpendUnconfirmed: true,
			},
			expectedErrSubstr: "SpendUnconfirmed is not supported",
		},
		{
			name: "target conf rejected",
			req: &lnrpc.OpenChannelRequest{
				TargetConf: 6,
			},
			expectedErrSubstr: "TargetConf is not supported",
		},
		{
			name: "sat per byte rejected",
			req: &lnrpc.OpenChannelRequest{
				SatPerByte: 10, //nolint:staticcheck
			},
			expectedErrSubstr: "SatPerByte is deprecated",
		},
		{
			name: "node pubkey string rejected",
			req: &lnrpc.OpenChannelRequest{
				NodePubkeyString: "abc", //nolint:staticcheck
			},
			expectedErrSubstr: "NodePubkeyString is not supported",
		},
		{
			name: "funding shim rejected",
			req: &lnrpc.OpenChannelRequest{
				FundingShim: &lnrpc.FundingShim{},
			},
			expectedErrSubstr: "FundingShim is not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInitialPsbtFlags(tc.req)
			if tc.expectedErrSubstr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tc.expectedErrSubstr)
		})
	}
}

// TestResolveCommitmentType verifies that supported commitment types are
// resolved correctly and unsupported types are rejected.
func TestResolveCommitmentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		commitmentType    lnrpc.CommitmentType
		expectedType      lnrpc.CommitmentType
		expectedErrSubstr string
	}{
		{
			name:           "unknown defaults to static remote key",
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedType:   lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		},
		{
			name:           "static remote key supported",
			commitmentType: lnrpc.CommitmentType_STATIC_REMOTE_KEY,
			expectedType:   lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		},
		{
			name:           "anchors supported",
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			expectedType:   lnrpc.CommitmentType_ANCHORS,
		},
		{
			name:           "simple taproot supported",
			commitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
			expectedType:   lnrpc.CommitmentType_SIMPLE_TAPROOT,
		},
		{
			name:              "legacy rejected",
			commitmentType:    lnrpc.CommitmentType_LEGACY,
			expectedErrSubstr: "unsupported commitment type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			commitmentType, err := resolveCommitmentType(
				tc.commitmentType,
			)
			if tc.expectedErrSubstr == "" {
				require.NoError(t, err)
				require.Equal(t, tc.expectedType, commitmentType)
				return
			}

			require.ErrorContains(t, err, tc.expectedErrSubstr)
		})
	}
}

// ---------------------------------------------------------------------------
// Mock types for PSBT channel open flow tests.
// ---------------------------------------------------------------------------

// mockLndClient implements lndclient.LightningClient for testing. Embedding
// the interface means unimplemented methods panic if called, which is
// desirable in tests to surface unexpected interactions.
type mockLndClient struct {
	lndclient.LightningClient

	rawClient lnrpc.LightningClient

	mu                 sync.Mutex
	fundingStepIdx     int
	fundingStepErr     error
	fundingStepCtxErrs []error
}

func (m *mockLndClient) RawClientWithMacAuth(
	ctx context.Context) (context.Context, time.Duration,
	lnrpc.LightningClient) {

	return ctx, 0, m.rawClient
}

func (m *mockLndClient) FundingStateStep(ctx context.Context,
	_ *lnrpc.FundingTransitionMsg) (*lnrpc.FundingStateStepResp, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.fundingStepIdx++
	m.fundingStepCtxErrs = append(m.fundingStepCtxErrs, ctx.Err())

	return &lnrpc.FundingStateStepResp{}, m.fundingStepErr
}

// mockRawLnrpcClient implements the raw gRPC lnrpc.LightningClient.
type mockRawLnrpcClient struct {
	lnrpc.LightningClient

	stream  lnrpc.Lightning_OpenChannelClient
	openErr error
}

func (m *mockRawLnrpcClient) OpenChannel(_ context.Context,
	_ *lnrpc.OpenChannelRequest,
	_ ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {

	return m.stream, m.openErr
}

// mockClientStream implements grpc.ClientStream for embedding in
// mockOpenChanStream.
type mockClientStream struct{}

func (m *mockClientStream) Header() (metadata.MD, error) {
	return nil, nil
}
func (m *mockClientStream) Trailer() metadata.MD { return nil }
func (m *mockClientStream) CloseSend() error     { return nil }
func (m *mockClientStream) Context() context.Context {
	return context.Background()
}
func (m *mockClientStream) SendMsg(_ interface{}) error { return nil }
func (m *mockClientStream) RecvMsg(_ interface{}) error { return nil }

// mockOpenChanStream implements lnrpc.Lightning_OpenChannelClient. It returns
// queued messages from Recv(), then returns finalErr once the queue is
// exhausted.
type mockOpenChanStream struct {
	*mockClientStream

	mu       sync.Mutex
	msgs     []*lnrpc.OpenStatusUpdate
	finalErr error
	idx      int
}

func (m *mockOpenChanStream) Recv() (*lnrpc.OpenStatusUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.idx >= len(m.msgs) {
		return nil, m.finalErr
	}

	msg := m.msgs[m.idx]
	m.idx++

	return msg, nil
}

// mockWithdrawManager implements the WithdrawalManager interface.
type mockWithdrawManager struct {
	tx   *wire.MsgTx
	psbt []byte
	err  error
}

func (m *mockWithdrawManager) CreateFinalizedWithdrawalTx(
	_ context.Context, _ []*deposit.Deposit,
	_ btcutil.Address, _ chainfee.SatPerKWeight, _ int64,
	_ lnrpc.CommitmentType) (*wire.MsgTx, []byte, error) {

	return m.tx, m.psbt, m.err
}

// testFundingAddress returns a valid regtest P2WPKH address for use in tests.
func testFundingAddress() string {
	addr, _ := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)

	return addr.EncodeAddress()
}

// ---------------------------------------------------------------------------
// Stream-level tests for the PSBT channel open flow.
// ---------------------------------------------------------------------------

// TestStreamOpenError verifies that when the lnd OpenChannel stream fails to
// open, the error is returned and the shim is cleaned up.
func TestStreamOpenError(t *testing.T) {
	t.Parallel()

	mockRaw := &mockRawLnrpcClient{
		openErr: errors.New("connection refused"),
	}
	lnClient := &mockLndClient{rawClient: mockRaw}

	manager := &Manager{
		cfg: &Config{
			LightningClient: lnClient,
			ChainParams:     &chaincfg.RegressionNetParams,
		},
	}

	req := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 100000,
		MinConfs:           defaultUtxoMinConf,
	}

	_, err := manager.openChannelPsbt(
		context.Background(), req, nil, 0,
	)
	require.ErrorContains(t, err, "opening stream to server failed")
	require.False(t, errors.Is(err, errPsbtFinalized))

	// Verify that the shim was canceled via FundingStateStep.
	lnClient.mu.Lock()
	require.Equal(t, 1, lnClient.fundingStepIdx)
	require.Len(t, lnClient.fundingStepCtxErrs, 1)
	require.NoError(t, lnClient.fundingStepCtxErrs[0])
	lnClient.mu.Unlock()
}

// TestStreamOpenErrorWithCanceledContext verifies that deferred shim cleanup
// still uses a live context even if the caller canceled the original request.
func TestStreamOpenErrorWithCanceledContext(t *testing.T) {
	t.Parallel()

	mockRaw := &mockRawLnrpcClient{
		openErr: errors.New("connection refused"),
	}
	lnClient := &mockLndClient{rawClient: mockRaw}

	manager := &Manager{
		cfg: &Config{
			LightningClient: lnClient,
			ChainParams:     &chaincfg.RegressionNetParams,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 100000,
		MinConfs:           defaultUtxoMinConf,
	}

	_, err := manager.openChannelPsbt(ctx, req, nil, 0)
	require.ErrorContains(t, err, "opening stream to server failed")

	lnClient.mu.Lock()
	require.Equal(t, 1, lnClient.fundingStepIdx)
	require.Len(t, lnClient.fundingStepCtxErrs, 1)
	require.NoError(t, lnClient.fundingStepCtxErrs[0])
	lnClient.mu.Unlock()
}

// TestStreamErrorBeforePsbtFinalize verifies that when the lnd stream returns
// an error before the PSBT is finalized, deposits are NOT wrapped in
// errPsbtFinalized so the caller can safely roll them back.
func TestStreamErrorBeforePsbtFinalize(t *testing.T) {
	t.Parallel()

	stream := &mockOpenChanStream{
		mockClientStream: &mockClientStream{},
		finalErr:         errors.New("peer disconnected"),
	}
	mockRaw := &mockRawLnrpcClient{stream: stream}
	lnClient := &mockLndClient{rawClient: mockRaw}

	manager := &Manager{
		cfg: &Config{
			LightningClient: lnClient,
			ChainParams:     &chaincfg.RegressionNetParams,
		},
	}

	req := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 100000,
		MinConfs:           defaultUtxoMinConf,
	}

	_, err := manager.openChannelPsbt(
		context.Background(), req, nil, 0,
	)
	require.Error(t, err)
	require.False(t, errors.Is(err, errPsbtFinalized))
}

// TestPsbtFinalizeThenStreamAbort verifies that when the PSBT finalize step
// succeeds but the stream dies before ChanPending, the error is wrapped with
// errPsbtFinalized so that the caller knows deposits must not be blindly
// rolled back.
func TestPsbtFinalizeThenStreamAbort(t *testing.T) {
	t.Parallel()

	fundingAmt := int64(100000)
	fundingAddr := testFundingAddress()

	stream := &mockOpenChanStream{
		mockClientStream: &mockClientStream{},
		msgs: []*lnrpc.OpenStatusUpdate{
			{
				Update: &lnrpc.OpenStatusUpdate_PsbtFund{
					PsbtFund: &lnrpc.ReadyForPsbtFunding{
						FundingAmount:  fundingAmt,
						FundingAddress: fundingAddr,
					},
				},
			},
		},
		finalErr: errors.New("stream died after finalize"),
	}
	mockRaw := &mockRawLnrpcClient{stream: stream}
	lnClient := &mockLndClient{rawClient: mockRaw}

	// Provide a minimal transaction that can be serialized.
	withdrawMgr := &mockWithdrawManager{
		tx:   wire.NewMsgTx(2),
		psbt: []byte("unsigned-psbt"),
	}

	manager := &Manager{
		cfg: &Config{
			LightningClient:   lnClient,
			WithdrawalManager: withdrawMgr,
			ChainParams:       &chaincfg.RegressionNetParams,
		},
	}

	req := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: fundingAmt,
		MinConfs:           defaultUtxoMinConf,
	}

	_, err := manager.openChannelPsbt(
		context.Background(), req, nil, 0,
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, errPsbtFinalized))

	// FundingStateStep should have been called 3 times: verify, finalize,
	// and shim cancel (from defer).
	lnClient.mu.Lock()
	require.Equal(t, 3, lnClient.fundingStepIdx)
	lnClient.mu.Unlock()
}
