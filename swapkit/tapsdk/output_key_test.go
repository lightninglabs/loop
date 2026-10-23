package tapsdk

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

type declaredOutputClient struct {
	sdk.Client

	key sdk.ScriptKey
}

func (c *declaredOutputClient) DeclareScriptKey(_ context.Context,
	request *sdk.DeclareScriptKeyRequest) (*sdk.ScriptKey, error) {

	c.key = request.ScriptKey
	return &c.key, nil
}

// TestDeclareOutputPreservesKey checks that declaring the native OP_TRUE output
// retains both compressed-key parity and the wallet locator. In particular, an
// even key must not be first registered with the control block's zero locator:
// tapd retains that first derivation when the same key later receives a refund.
func TestDeclareOutputPreservesKey(t *testing.T) {
	for name, scalar := range map[string]byte{testEven: 1, "odd": 6} {
		t.Run(name, func(t *testing.T) {
			fixture := newCustomAnchorBuilderFixture(t)
			kit, err := swapkit.NewSwapKit(swapKitTestParams(t))
			require.NoError(t, err)
			key := sdk.KeyDescriptor{
				RawKeyBytes: testKey(t, scalar),
				KeyLocator: sdk.KeyLocator{
					Family: 42060, Index: 7,
				},
			}
			if name == testEven {
				require.Equal(t, byte(2), key.RawKeyBytes[0])
			} else {
				require.Equal(t, byte(3), key.RawKeyBytes[0])
			}
			fixture.request.Outputs[0] = OPTrueOutput(
				fixture.request.Outputs[0],
				key,
				kit.AnchorPlan(),
			)
			wallet := sdk.NewWallet(
				fixture.client,
				sdk.NetworkRegtest,
			)
			plan, err := wallet.NewCustomAnchorTxBuilder().Build(
				t.Context(), fixture.request,
			)
			require.NoError(t, err)
			out := plan.Outputs()[0]
			pkg := &sdk.CustomAnchorTransferPackage{
				ActiveVirtualPsbts: plan.ActiveVirtualPSBTs(),
			}
			client := &declaredOutputClient{}
			wallet = sdk.NewWallet(client, sdk.NetworkRegtest)
			require.NoError(
				t,
				declareOutput(t.Context(), wallet, pkg,
					sdk.CustomAnchorAssetOutputSummary{
						PacketIndex:        out.PacketIndex,
						VirtualOutputIndex: out.VirtualOutputIndex,
						ScriptKey:          out.ScriptKey,
					}),
			)
			require.Equal(t, key, client.key.KeyDesc)
			require.Equal(t, out.ScriptKey, client.key.PubKey)
			require.Len(t, client.key.TapTweak, 32)
		})
	}
}
