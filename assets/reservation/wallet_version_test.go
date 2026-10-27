package reservation

import (
	"context"
	"testing"

	"github.com/lightninglabs/lndclient"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type versionedProofNode struct {
	ProofVerifier

	version string
}

const testTapdVersion = "v0.8.6"

func (v versionedProofNode) GetInfo(context.Context,
	*taprpc.GetInfoRequest, ...grpc.CallOption) (
	*taprpc.GetInfoResponse, error) {

	return &taprpc.GetInfoResponse{Version: v.version}, nil
}

type reservationTestKeys struct {
	called bool
}

func (k *reservationTestKeys) DeriveNextKey(context.Context, int32) (
	*keychain.KeyDescriptor, error) {

	k.called = true
	return &keychain.KeyDescriptor{}, nil
}

func (k *reservationTestKeys) DeriveKey(context.Context,
	*keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	return nil, nil
}

// TestNewPurchaseTapdFloor checks that a new reservation cannot start with an
// old tapd while existing reservation recovery remains a separate operation.
func TestNewPurchaseTapdFloor(t *testing.T) {
	keys := &reservationTestKeys{}
	wallet := &WalletVerifier{
		Keys: keys, Params: &address.RegressionNetTap, KeyFamily: 1,
		Chain: &ChainReader{
			Chain: struct{ ChainSource }{},
			Node:  confirmationNode{},
			ChainNotifier: &struct {
				lndclient.ChainNotifierClient
			}{},
		},
	}
	wallet.Tap = versionedProofNode{version: "v0.8.3"}
	_, err := wallet.DeriveKey(t.Context())
	require.ErrorIs(t, err, assetpolicy.ErrTapdVersionUnsupported)
	require.False(t, keys.called)

	wallet.Tap = versionedProofNode{version: testTapdVersion}
	_, err = wallet.DeriveKey(t.Context())
	require.NoError(t, err)
	require.True(t, keys.called)
}
