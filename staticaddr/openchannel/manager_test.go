package openchannel

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
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

func testOutPoint(b byte) wire.OutPoint {
	return wire.OutPoint{
		Hash:  chainhash.Hash{b},
		Index: uint32(b),
	}
}

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

func TestValidateInitialPsbtFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		minConfs          int32
		spendUnconfirmed  bool
		expectedErrSubstr string
	}{
		{
			name:             "default min confs accepted",
			minConfs:         0,
			spendUnconfirmed: false,
		},
		{
			name:             "explicit default min confs accepted",
			minConfs:         defaultUtxoMinConf,
			spendUnconfirmed: false,
		},
		{
			name:              "custom min confs rejected",
			minConfs:          defaultUtxoMinConf + 1,
			spendUnconfirmed:  false,
			expectedErrSubstr: "custom MinConfs not supported",
		},
		{
			name:              "spend unconfirmed rejected",
			minConfs:          defaultUtxoMinConf,
			spendUnconfirmed:  true,
			expectedErrSubstr: "SpendUnconfirmed is not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &lnrpc.OpenChannelRequest{
				MinConfs:         tc.minConfs,
				SpendUnconfirmed: tc.spendUnconfirmed,
			}

			err := validateInitialPsbtFlags(req)
			if tc.expectedErrSubstr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tc.expectedErrSubstr)
		})
	}
}

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
