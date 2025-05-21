package deposit

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var (
	defaultServerPubkeyBytes, _ = hex.DecodeString("021c97a90a411ff2b10dc2a8e32de2f29d2fa49d41bfbb52bd416e460db0747d0d")

	defaultServerPubkey, _ = btcec.ParsePubKey(defaultServerPubkeyBytes)

	defaultExpiry = uint32(100)

	defaultDepositConfirmations = uint32(3)

	confChan = make(chan *chainntnfs.TxConfirmation)

	confErrChan = make(chan error)

	blockChan = make(chan int32)

	blockErrChan = make(chan error)

	finalizedDepositChan = make(chan wire.OutPoint)
)

type mockStaticAddressClient struct {
	mock.Mock
}

func (m *mockStaticAddressClient) ServerStaticAddressLoopIn(ctx context.Context,
	in *swapserverrpc.ServerStaticAddressLoopInRequest,
	opts ...grpc.CallOption) (
	*swapserverrpc.ServerStaticAddressLoopInResponse, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.ServerStaticAddressLoopInResponse),
		args.Error(1)
}

func (m *mockStaticAddressClient) PushStaticAddressSweeplessSigs(ctx context.Context,
	in *swapserverrpc.PushStaticAddressSweeplessSigsRequest,
	opts ...grpc.CallOption) (
	*swapserverrpc.PushStaticAddressSweeplessSigsResponse, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.PushStaticAddressSweeplessSigsResponse),
		args.Error(1)
}

func (m *mockStaticAddressClient) PushStaticAddressHtlcSigs(ctx context.Context,
	in *swapserverrpc.PushStaticAddressHtlcSigsRequest,
	opts ...grpc.CallOption) (
	*swapserverrpc.PushStaticAddressHtlcSigsResponse, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.PushStaticAddressHtlcSigsResponse),
		args.Error(1)
}

func (m *mockStaticAddressClient) ServerWithdrawDeposits(ctx context.Context,
	in *swapserverrpc.ServerWithdrawRequest,
	opts ...grpc.CallOption) (*swapserverrpc.ServerWithdrawResponse,
	error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.ServerWithdrawResponse),
		args.Error(1)
}

func (m *mockStaticAddressClient) ServerPsbtWithdrawDeposits(ctx context.Context,
	in *swapserverrpc.ServerPsbtWithdrawRequest,
	opts ...grpc.CallOption) (*swapserverrpc.ServerPsbtWithdrawResponse,
	error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.ServerPsbtWithdrawResponse),
		args.Error(1)
}

func (m *mockStaticAddressClient) ServerNewAddress(ctx context.Context,
	in *swapserverrpc.ServerNewAddressRequest, opts ...grpc.CallOption) (
	*swapserverrpc.ServerNewAddressResponse, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.ServerNewAddressResponse),
		args.Error(1)
}

type mockAddressManager struct {
	mock.Mock
}

func (m *mockAddressManager) GetStaticAddressParameters(ctx context.Context) (
	*address.Parameters, error) {

	args := m.Called(ctx)

	return args.Get(0).(*address.Parameters),
		args.Error(1)
}

func (m *mockAddressManager) GetStaticAddress(ctx context.Context) (
	*script.StaticAddress, error) {

	args := m.Called(ctx)

	return args.Get(0).(*script.StaticAddress),
		args.Error(1)
}

func (m *mockAddressManager) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*lnwallet.Utxo, error) {

	args := m.Called(ctx, minConfs, maxConfs)

	return args.Get(0).([]*lnwallet.Utxo),
		args.Error(1)
}

func (m *mockAddressManager) GetTaprootAddress(clientPubkey,
	serverPubkey *btcec.PublicKey, expiry int64) (*btcutil.AddressTaproot,
	error) {

	args := m.Called(clientPubkey, serverPubkey, expiry)

	return args.Get(0).(*btcutil.AddressTaproot),
		args.Error(1)
}

type mockStore struct {
	mock.Mock
}

func (s *mockStore) CreateDeposit(ctx context.Context, deposit *Deposit) error {
	args := s.Called(ctx, deposit)
	return args.Error(0)
}

func (s *mockStore) UpdateDeposit(ctx context.Context, deposit *Deposit) error {
	args := s.Called(ctx, deposit)
	return args.Error(0)
}

func (s *mockStore) GetDeposit(ctx context.Context, depositID ID) (*Deposit,
	error) {

	args := s.Called(ctx, depositID)
	return args.Get(0).(*Deposit), args.Error(1)
}

func (s *mockStore) DepositForOutpoint(ctx context.Context,
	outpoint string) (*Deposit, error) {

	args := s.Called(ctx, outpoint)
	return args.Get(0).(*Deposit), args.Error(1)
}

func (s *mockStore) AllDeposits(ctx context.Context) ([]*Deposit, error) {
	args := s.Called(ctx)
	return args.Get(0).([]*Deposit), args.Error(1)
}

type MockChainNotifier struct {
	mock.Mock
}

func (m *MockChainNotifier) RawClientWithMacAuth(
	ctx context.Context) (context.Context, time.Duration,
	chainrpc.ChainNotifierClient) {

	return ctx, 0, nil
}

func (m *MockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	args := m.Called(ctx, txid, pkScript, numConfs, heightHint)
	return args.Get(0).(chan *chainntnfs.TxConfirmation),
		args.Get(1).(chan error), args.Error(2)
}

func (m *MockChainNotifier) RegisterBlockEpochNtfn(ctx context.Context) (
	chan int32, chan error, error) {

	args := m.Called(ctx)
	return args.Get(0).(chan int32), args.Get(1).(chan error), args.Error(2)
}

func (m *MockChainNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	args := m.Called(ctx, pkScript, heightHint)
	return args.Get(0).(chan *chainntnfs.SpendDetail),
		args.Get(1).(chan error), args.Error(2)
}

// TestManager checks that the manager processes the right channel notifications
// while a deposit is expiring.
func TestManager(t *testing.T) {
	ctx := context.Background()

	// Create the test context with required mocks.
	testContext := newManagerTestContext(t)

	// Start the deposit manager.
	initChan := make(chan struct{})
	go func() {
		require.NoError(t, testContext.manager.Run(ctx, initChan))
	}()

	// Ensure that the manager has been initialized.
	<-initChan

	// Notify about the last block before the expiry.
	blockChan <- int32(defaultDepositConfirmations + defaultExpiry)

	// Ensure that the deposit state machine didn't sign for the expiry tx.
	select {
	case <-testContext.mockLnd.SignOutputRawChannel:
		t.Fatal("received unexpected sign request")

	default:
	}

	// Mine the expiry tx height.
	blockChan <- int32(defaultDepositConfirmations + defaultExpiry)

	// Ensure that the deposit state machine didn't sign for the expiry tx.
	<-testContext.mockLnd.SignOutputRawChannel

	// Ensure that the signed expiry transaction is published.
	expiryTx := <-testContext.mockLnd.TxPublishChannel

	// Ensure that the deposit is waiting for a confirmation notification.
	confChan <- &chainntnfs.TxConfirmation{
		BlockHeight: defaultDepositConfirmations + defaultExpiry + 3,
		Tx:          expiryTx,
	}

	// Ensure that the deposit is finalized.
	<-finalizedDepositChan
}

// ManagerTestContext is a helper struct that contains all the necessary
// components to test the reservation manager.
type ManagerTestContext struct {
	manager                 *Manager
	context                 test.Context
	mockLnd                 *test.LndMockServices
	mockStaticAddressClient *mockStaticAddressClient
	mockAddressManager      *mockAddressManager
}

// newManagerTestContext creates a new test context for the reservation manager.
func newManagerTestContext(t *testing.T) *ManagerTestContext {
	mockLnd := test.NewMockLnd()
	lndContext := test.NewContext(t, mockLnd)

	mockStaticAddressClient := new(mockStaticAddressClient)
	mockAddressManager := new(mockAddressManager)
	mockStore := new(mockStore)
	mockChainNotifier := new(MockChainNotifier)

	ID, err := GetRandomDepositID()
	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.TaprootPubkey,
		Value:         btcutil.Amount(100000),
		Confirmations: int64(defaultDepositConfirmations),
		PkScript:      []byte("pkscript"),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0xffffffff,
		},
	}
	require.NoError(t, err)
	storedDeposits := []*Deposit{
		{
			ID:                   ID,
			state:                Deposited,
			OutPoint:             utxo.OutPoint,
			Value:                utxo.Value,
			ConfirmationHeight:   3,
			TimeOutSweepPkScript: []byte{0x42, 0x21, 0x69},
		},
	}

	mockStore.On(
		"AllDeposits", mock.Anything,
	).Return(storedDeposits, nil)

	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil)

	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return(&address.Parameters{
		Expiry: defaultExpiry,
	}, nil)

	mockAddressManager.On(
		"ListUnspent", mock.Anything, mock.Anything, mock.Anything,
	).Return([]*lnwallet.Utxo{utxo}, nil)

	// Define the expected return values for the mocks.
	mockChainNotifier.On(
		"RegisterConfirmationsNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(confChan, confErrChan, nil)

	mockChainNotifier.On("RegisterBlockEpochNtfn", mock.Anything).Return(
		blockChan, blockErrChan, nil,
	)

	cfg := &ManagerConfig{
		AddressClient:  mockStaticAddressClient,
		AddressManager: mockAddressManager,
		Store:          mockStore,
		WalletKit:      mockLnd.WalletKit,
		ChainParams:    mockLnd.ChainParams,
		ChainNotifier:  mockChainNotifier,
		Signer:         mockLnd.Signer,
	}

	manager := NewManager(cfg)
	manager.finalizedDepositChan = finalizedDepositChan

	testContext := &ManagerTestContext{
		manager:                 manager,
		context:                 lndContext,
		mockLnd:                 mockLnd,
		mockStaticAddressClient: mockStaticAddressClient,
		mockAddressManager:      mockAddressManager,
	}

	staticAddress := generateStaticAddress(
		context.Background(), testContext,
	)
	mockAddressManager.On(
		"GetStaticAddress", mock.Anything,
	).Return(staticAddress, nil)

	return testContext
}

func generateStaticAddress(ctx context.Context,
	t *ManagerTestContext) *script.StaticAddress {

	keyDescriptor, err := t.mockLnd.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	require.NoError(t.context.T, err)

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry),
		keyDescriptor.PubKey, defaultServerPubkey,
	)
	require.NoError(t.context.T, err)

	return staticAddress
}
