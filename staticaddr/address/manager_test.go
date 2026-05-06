package address

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/lndclient"
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

type failingImportWalletKit struct {
	lndclient.WalletKitClient

	err error
}

func (w *failingImportWalletKit) ImportTaprootScript(context.Context,
	*waddrmgr.Tapscript) (btcutil.Address, error) {

	return nil, w.err
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

// ServerWithdrawDeposits implements the deprecated RPC required by the
// generated client interface. Production code uses ServerPsbtWithdrawDeposits.
//
//nolint:staticcheck
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

	resp, _ := args.Get(0).(*swapserverrpc.ServerNewAddressResponse)

	return resp, args.Error(1)
}

// TestManager tests the static address manager generates the corerct static
// taproot address from the given test parameters.
func TestManager(t *testing.T) {
	ctxb := t.Context()

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

	storedParams, err := testContext.manager.GetStaticAddressParameters(ctxb)
	require.NoError(t, err)
	require.EqualValues(
		t, swap.StaticAddressKeyFamily, storedParams.KeyLocator.Family,
	)

	addresses, err := testContext.manager.GetAllAddresses(ctxb)
	require.NoError(t, err)
	require.Len(t, addresses, 2)
	require.EqualValues(
		t, swap.StaticMultiAddressKeyFamily,
		addresses[1].KeyLocator.Family,
	)
}

// TestRestoreAddress verifies that restoring an address recreates the same
// static address locally without requiring a server call.
func TestRestoreAddress(t *testing.T) {
	ctxb := t.Context()

	testContext := NewAddressManagerTestContext(t)

	keyDesc, err := testContext.mockLnd.WalletKit.DeriveKey(
		ctxb, &keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  7,
		},
	)
	require.NoError(t, err)

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry),
		keyDesc.PubKey, defaultServerPubkey,
	)
	require.NoError(t, err)

	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	addressParams := &Parameters{
		ClientPubkey:     keyDesc.PubKey,
		ServerPubkey:     defaultServerPubkey,
		Expiry:           defaultExpiry,
		PkScript:         pkScript,
		KeyLocator:       keyDesc.KeyLocator,
		ProtocolVersion:  0,
		InitiationHeight: 123,
	}

	taprootAddress, restored, err := testContext.manager.RestoreAddress(
		ctxb, addressParams,
	)
	require.NoError(t, err)
	require.True(t, restored)

	expectedAddress, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(staticAddress.TaprootKey),
		testContext.manager.cfg.ChainParams,
	)
	require.NoError(t, err)
	require.Equal(t, expectedAddress.String(), taprootAddress.String())

	storedParams, err := testContext.manager.GetStaticAddressParameters(ctxb)
	require.NoError(t, err)
	require.True(t, sameAddressParameters(storedParams, addressParams))

	taprootAddress, restored, err = testContext.manager.RestoreAddress(
		ctxb, addressParams,
	)
	require.NoError(t, err)
	require.False(t, restored)
	require.Equal(t, expectedAddress.String(), taprootAddress.String())
}

// TestRestoreAddressImportFailureDoesNotCreateRow verifies that a failed lnd
// tapscript import leaves no static-address DB row behind, so a later retry can
// restore cleanly.
func TestRestoreAddressImportFailureDoesNotCreateRow(t *testing.T) {
	ctxb := t.Context()

	testContext := NewAddressManagerTestContext(t)

	keyDesc, err := testContext.mockLnd.WalletKit.DeriveKey(
		ctxb, &keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  7,
		},
	)
	require.NoError(t, err)

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry),
		keyDesc.PubKey, defaultServerPubkey,
	)
	require.NoError(t, err)

	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	addressParams := &Parameters{
		ClientPubkey:     keyDesc.PubKey,
		ServerPubkey:     defaultServerPubkey,
		Expiry:           defaultExpiry,
		PkScript:         pkScript,
		KeyLocator:       keyDesc.KeyLocator,
		ProtocolVersion:  0,
		InitiationHeight: 123,
	}

	importErr := errors.New("import failed")
	testContext.manager.cfg.WalletKit = &failingImportWalletKit{
		WalletKitClient: testContext.mockLnd.WalletKit,
		err:             importErr,
	}

	_, _, err = testContext.manager.RestoreAddress(ctxb, addressParams)
	require.ErrorIs(t, err, importErr)

	_, err = testContext.manager.GetStaticAddressParameters(ctxb)
	require.ErrorIs(t, err, ErrNoStaticAddress)

	testContext.manager.cfg.WalletKit = testContext.mockLnd.WalletKit
	_, restored, err := testContext.manager.RestoreAddress(
		ctxb, addressParams,
	)
	require.NoError(t, err)
	require.True(t, restored)
}

// TestRestoreAddressRejectsDifferentInitiationHeight verifies that a restore
// request with the same address material but a different initiation height is
// rejected instead of being treated as idempotent.
func TestRestoreAddressRejectsDifferentInitiationHeight(t *testing.T) {
	ctxb := t.Context()

	testContext := NewAddressManagerTestContext(t)

	keyDesc, err := testContext.mockLnd.WalletKit.DeriveKey(
		ctxb, &keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  7,
		},
	)
	require.NoError(t, err)

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry),
		keyDesc.PubKey, defaultServerPubkey,
	)
	require.NoError(t, err)

	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	addressParams := &Parameters{
		ClientPubkey:     keyDesc.PubKey,
		ServerPubkey:     defaultServerPubkey,
		Expiry:           defaultExpiry,
		PkScript:         pkScript,
		KeyLocator:       keyDesc.KeyLocator,
		ProtocolVersion:  0,
		InitiationHeight: 123,
	}

	_, _, err = testContext.manager.RestoreAddress(ctxb, addressParams)
	require.NoError(t, err)

	differentHeight := *addressParams
	differentHeight.InitiationHeight = 456

	_, _, err = testContext.manager.RestoreAddress(ctxb, &differentHeight)
	require.ErrorContains(t, err, "existing static address differs from backup")
}

// TestNewAddressValidatesServerResponse tests that the untrusted
// ServerNewAddress response is validated before the address script is created.
func TestNewAddressValidatesServerResponse(t *testing.T) {
	tests := []struct {
		name     string
		resp     *swapserverrpc.ServerNewAddressResponse
		expected string
	}{
		{
			name:     "nil response",
			expected: "missing server new address response",
		},
		{
			name:     "nil params",
			resp:     &swapserverrpc.ServerNewAddressResponse{},
			expected: "missing server address parameters",
		},
		{
			name: "missing server key",
			resp: &swapserverrpc.ServerNewAddressResponse{
				Params: &swapserverrpc.ServerAddressParameters{
					Expiry: defaultExpiry,
				},
			},
			expected: "missing server public key",
		},
		{
			name: "uncompressed server key",
			resp: &swapserverrpc.ServerNewAddressResponse{
				Params: &swapserverrpc.ServerAddressParameters{
					ServerKey: []byte{0x04},
					Expiry:    defaultExpiry,
				},
			},
			expected: "server public key is not a compressed",
		},
		{
			name:     "zero expiry",
			resp:     newServerNewAddressResponse(0),
			expected: "static address CSV expiry must be non-zero",
		},
		{
			name: "seconds flag",
			resp: newServerNewAddressResponse(
				wire.SequenceLockTimeIsSeconds | 1,
			),
			expected: "static address expiry does not fit into CSV",
		},
		{
			name: "disabled flag",
			resp: newServerNewAddressResponse(
				wire.SequenceLockTimeDisabled | 1,
			),
			expected: "static address expiry does not fit into CSV",
		},
		{
			name: "reserved flag",
			resp: newServerNewAddressResponse(
				wire.SequenceLockTimeMask + 1,
			),
			expected: "static address expiry does not fit into CSV",
		},
		{
			name: "too large",
			resp: newServerNewAddressResponse(
				maxStaticAddressCSVExpiry + 1,
			),
			expected: "exceeds maximum",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testContext := NewAddressManagerTestContextWithResponse(
				t, test.resp,
			)

			_, _, err := testContext.manager.NewAddress(t.Context())
			require.ErrorContains(t, err, test.expected)
		})
	}
}

// TestNewAddressAcceptsMaxCSVExpiry tests the upper valid CSV boundary.
func TestNewAddressAcceptsMaxCSVExpiry(t *testing.T) {
	testContext := NewAddressManagerTestContextWithResponse(
		t, newServerNewAddressResponse(maxStaticAddressCSVExpiry),
	)

	_, expiry, err := testContext.manager.NewAddress(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, maxStaticAddressCSVExpiry, expiry)
}

// GenerateExpectedTaprootAddress generates the expected taproot address that
// the predefined parameters are supposed to generate.
func GenerateExpectedTaprootAddress(t *ManagerTestContext) (
	*btcutil.AddressTaproot, error) {

	keyIndex := int32(1)
	_, pubKey := test.CreateKey(keyIndex)

	keyDescriptor := &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticMultiAddressKeyFamily),
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
	return NewAddressManagerTestContextWithResponse(
		t, newServerNewAddressResponse(defaultExpiry),
	)
}

// NewAddressManagerTestContextWithResponse creates a new test context with a
// custom ServerNewAddress response.
func NewAddressManagerTestContextWithResponse(t *testing.T,
	resp *swapserverrpc.ServerNewAddressResponse) *ManagerTestContext {

	ctxb, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockLnd := test.NewMockLnd()
	lndContext := test.NewContext(t, mockLnd)

	dbFixture := loopdb.NewTestDB(t)

	store := NewSqlStore(dbFixture.BaseDB)

	mockStaticAddressClient := new(mockStaticAddressClient)

	mockStaticAddressClient.On(
		"ServerNewAddress", mock.Anything, mock.Anything, mock.Anything,
	).Return(resp, nil)

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

	manager, err := NewManager(cfg, int32(getInfo.BlockHeight))
	require.NoError(t, err)

	return &ManagerTestContext{
		manager:                 manager,
		context:                 lndContext,
		mockLnd:                 mockLnd,
		mockStaticAddressClient: mockStaticAddressClient,
	}
}

// newServerNewAddressResponse returns a valid server response with the given
// CSV expiry.
func newServerNewAddressResponse(expiry uint32) *swapserverrpc.ServerNewAddressResponse {
	return &swapserverrpc.ServerNewAddressResponse{
		Params: &swapserverrpc.ServerAddressParameters{
			ServerKey: defaultServerPubkeyBytes,
			Expiry:    expiry,
		},
	}
}
