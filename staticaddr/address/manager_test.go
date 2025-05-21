package address

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var (
	defaultServerPubkeyBytes, _ = hex.DecodeString("021c97a90a411ff2b10dc2a8e32de2f29d2fa49d41bfbb52bd416e460db0747d0d")

	defaultServerPubkey, _ = btcec.ParsePubKey(defaultServerPubkeyBytes)

	defaultExpiry = uint32(100)
)

type mockStaticAddressClient struct {
	mock.Mock
}

func (m *mockStaticAddressClient) SignOpenChannelPsbt(ctx context.Context,
	in *swapserverrpc.SignOpenChannelPsbtRequest,
	opts ...grpc.CallOption) (
	*swapserverrpc.SignOpenChannelPsbtResponse, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.SignOpenChannelPsbtResponse),
		args.Error(1)
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

func (m *mockStaticAddressClient) ServerNewAddress(ctx context.Context,
	in *swapserverrpc.ServerNewAddressRequest, opts ...grpc.CallOption) (
	*swapserverrpc.ServerNewAddressResponse, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.ServerNewAddressResponse),
		args.Error(1)
}

// TestManager tests the static address manager generates the corerct static
// taproot address from the given test parameters.
func TestManager(t *testing.T) {
	ctxb, cancel := context.WithCancel(context.Background())
	defer cancel()

	testContext := NewAddressManagerTestContext(t)

	// Start the manager.
	initChan := make(chan struct{})
	go func() {
		err := testContext.manager.Run(ctxb, initChan)
		require.ErrorIs(t, err, context.Canceled)
	}()

	<-initChan

	// Create the expected static address.
	expectedAddress, err := GenerateExpectedTaprootAddress(testContext)
	require.NoError(t, err)

	// Create a new static address.
	taprootAddress, expiry, err := testContext.manager.NewAddress(ctxb)
	require.NoError(t, err)

	// The addresses have to match.
	require.Equal(t, expectedAddress.String(), taprootAddress.String())

	// The expiry has to match.
	require.EqualValues(t, defaultExpiry, expiry)
}

// GenerateExpectedTaprootAddress generates the expected taproot address that
// the predefined parameters are supposed to generate.
func GenerateExpectedTaprootAddress(t *ManagerTestContext) (
	*btcutil.AddressTaproot, error) {

	keyIndex := int32(0)
	_, pubKey := test.CreateKey(keyIndex)

	keyDescriptor := &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  uint32(keyIndex),
		},
		PubKey: pubKey,
	}

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry),
		keyDescriptor.PubKey, defaultServerPubkey,
	)
	if err != nil {
		return nil, err
	}

	return btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(staticAddress.TaprootKey),
		t.manager.cfg.ChainParams,
	)
}

// ManagerTestContext is a helper struct that contains all the necessary
// components to test the static address manager.
type ManagerTestContext struct {
	manager                 *Manager
	context                 test.Context
	mockLnd                 *test.LndMockServices
	mockStaticAddressClient *mockStaticAddressClient
}

// NewAddressManagerTestContext creates a new test context for the static
// address manager.
func NewAddressManagerTestContext(t *testing.T) *ManagerTestContext {
	ctxb, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLnd := test.NewMockLnd()
	lndContext := test.NewContext(t, mockLnd)

	dbFixture := loopdb.NewTestDB(t)

	store := NewSqlStore(dbFixture.BaseDB)

	mockStaticAddressClient := new(mockStaticAddressClient)

	mockStaticAddressClient.On(
		"ServerNewAddress", mock.Anything, mock.Anything, mock.Anything,
	).Return(
		&swapserverrpc.ServerNewAddressResponse{
			Params: &swapserverrpc.ServerAddressParameters{
				ServerKey: defaultServerPubkeyBytes,
				Expiry:    defaultExpiry,
			},
		}, nil,
	)

	cfg := &ManagerConfig{
		Store:         store,
		WalletKit:     mockLnd.WalletKit,
		ChainParams:   mockLnd.ChainParams,
		AddressClient: mockStaticAddressClient,
		ChainNotifier: mockLnd.ChainNotifier,
		FetchL402:     func(context.Context) error { return nil },
	}

	getInfo, err := mockLnd.Client.GetInfo(ctxb)
	require.NoError(t, err)

	manager := NewManager(cfg, int32(getInfo.BlockHeight))

	return &ManagerTestContext{
		manager:                 manager,
		context:                 lndContext,
		mockLnd:                 mockLnd,
		mockStaticAddressClient: mockStaticAddressClient,
	}
}
