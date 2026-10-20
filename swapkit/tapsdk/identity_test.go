package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

type identitySDK struct {
	sdk.Client

	info *sdk.Info
}

func (s *identitySDK) GetInfo(context.Context) (*sdk.Info, error) {
	return s.info, nil
}

type identityChain struct{ Chain }

func (*identityChain) Height(context.Context) (uint32, error) {
	return 100, nil
}

type identityWallet struct{ lndclient.WalletKitClient }
type identitySigner struct{ lndclient.SignerClient }

func identityConfig(t *testing.T, scalar byte,
	network sdk.Network) DriverConfig {

	t.Helper()
	private, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	public, err := sdk.ParsePubKey(private.PubKey().SerializeCompressed())
	require.NoError(t, err)
	client := &identitySDK{info: &sdk.Info{
		LndIdentityPubkey: public, Network: string(network),
		SyncedToChain: true, BlockHeight: 100,
	}}
	return DriverConfig{
		Network: network, SDK: client, NodePubkey: public,
		Wallet:    sdk.NewWallet(client, network),
		WalletKit: &identityWallet{}, Signer: &identitySigner{},
		Chain: &identityChain{}, FeeSat: 1000, MaxFeeSat: 2000,
		MinConfs: 1,
	}
}

// TestReadyBindsNormalDaemonIdentity checks the authenticated tapd connection
// against the already-running daemon's signing identity and current network.
func TestReadyBindsNormalDaemonIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DriverConfig)
		err    error
	}{
		{name: "ready"},
		{name: "wrong wallet", mutate: func(cfg *DriverConfig) {
			cfg.NodePubkey = bindingPub(3)
		}, err: swapkit.ErrInvalid},
		{name: "wrong network", mutate: func(cfg *DriverConfig) {
			cfg.Network = sdk.NetworkMainnet
		}, err: swapkit.ErrInvalid},
		{name: "lagging tapd", mutate: func(cfg *DriverConfig) {
			cfg.SDK.(*identitySDK).info.BlockHeight--
		}, err: swapkit.ErrPending},
		{name: "unsynced tapd", mutate: func(cfg *DriverConfig) {
			cfg.SDK.(*identitySDK).info.SyncedToChain = false
		}, err: swapkit.ErrPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := identityConfig(t, 2, sdk.NetworkRegtest)
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			for range 2 {
				r, err := NewDriver(cfg)
				require.NoError(t, err)
				err = r.CheckReady(t.Context())
				if test.err == nil {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, test.err)
				}
			}
		})
	}
}
