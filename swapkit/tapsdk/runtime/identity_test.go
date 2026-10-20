package runtime

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapkit/tapsdk/walletsigner"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type identitySDK struct {
	sdk.Client
	info *sdk.Info
}

func (s *identitySDK) GetInfo(context.Context) (*sdk.Info, error) {
	return s.info, nil
}

type identityLND struct {
	lnrpc.LightningClient
	info *lnrpc.GetInfoResponse
}

func (s *identityLND) GetInfo(context.Context, *lnrpc.GetInfoRequest,
	...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	return s.info, nil
}

type identityChain struct{ Chain }

func (*identityChain) Height(context.Context) (uint32, error) {
	return 100, nil
}

type identityWallet struct{ walletrpc.WalletKitClient }

func identityConfig(t *testing.T, path string, scalar byte,
	network sdk.Network) Config {

	t.Helper()
	private, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	public, err := sdk.ParsePubKey(private.PubKey().SerializeCompressed())
	require.NoError(t, err)
	client := &identitySDK{info: &sdk.Info{
		LndIdentityPubkey: public, Network: string(network),
		SyncedToChain: true, BlockHeight: 100,
	}}
	return Config{
		Path: path, Network: network, SDK: client,
		Wallet: sdk.NewWallet(client, network),
		Lightning: &identityLND{info: &lnrpc.GetInfoResponse{
			IdentityPubkey: hex.EncodeToString(public[:]),
			SyncedToChain:  true, BlockHeight: 100,
		}},
		WalletKit: &identityWallet{}, Signer: &walletsigner.Signer{},
		Chain: &identityChain{}, FeeSat: 1000, MaxFeeSat: 2000,
		MinConfs: 1,
	}
}

func TestJournalRejectsDifferentWalletAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	cfg := identityConfig(t, path, 2, sdk.NetworkRegtest)
	r, err := Open(cfg)
	require.NoError(t, err)
	_, err = r.Call(t.Context(), bridge.CheckReady, nil)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	for _, cfg := range []Config{
		identityConfig(t, path, 3, sdk.NetworkRegtest),
		identityConfig(t, path, 2, sdk.NetworkMainnet),
	} {
		r, err := Open(cfg)
		require.NoError(t, err)
		_, err = r.Call(t.Context(), bridge.CheckReady, nil)
		require.ErrorIs(t, err, bridge.ErrInvalid)
		require.NoError(t, r.Close())
	}
	r, err = Open(cfg)
	require.NoError(t, err)
	defer r.Close()
	_, err = r.Call(t.Context(), bridge.CheckReady, nil)
	require.NoError(t, err)
}

func TestPendingReadinessDoesNotBindJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	cfg := identityConfig(t, path, 2, sdk.NetworkRegtest)
	cfg.SDK.(*identitySDK).info.BlockHeight--
	r, err := Open(cfg)
	require.NoError(t, err)
	_, err = r.Call(t.Context(), bridge.CheckReady, nil)
	require.ErrorIs(t, err, bridge.ErrPending)
	var identity bridge.Ready
	require.True(t, missing(r.get("identity/v1", &identity)))
	require.NoError(t, r.Close())
	r, err = Open(identityConfig(t, path, 3, sdk.NetworkRegtest))
	require.NoError(t, err)
	defer r.Close()
	_, err = r.Call(t.Context(), bridge.CheckReady, nil)
	require.NoError(t, err)
}

func TestJournalRequiresPrivateRegularFile(t *testing.T) {
	for _, mode := range []os.FileMode{0400, 0640, 0644} {
		path := filepath.Join(t.TempDir(), "runtime.db")
		require.NoError(t, os.WriteFile(path, nil, mode))
		_, err := Open(identityConfig(t, path, 2, sdk.NetworkRegtest))
		require.ErrorIs(t, err, bridge.ErrInvalid)
	}
	path := filepath.Join(t.TempDir(), "runtime.db")
	r, err := Open(identityConfig(t, path, 2, sdk.NetworkRegtest))
	require.NoError(t, err)
	require.NoError(t, r.Close())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	link := filepath.Join(t.TempDir(), "runtime.db")
	require.NoError(t, os.Symlink(path, link))
	_, err = Open(identityConfig(t, link, 2, sdk.NetworkRegtest))
	require.ErrorIs(t, err, bridge.ErrInvalid)
}
