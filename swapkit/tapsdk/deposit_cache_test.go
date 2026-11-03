package tapsdk

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKBuilderOwnsInputVerification covers the real SDK builder call. The
// Loop checks on each side must not call verifyProof a second time, and the
// post-build check rejects chain changes during the SDK verification RPC.
func TestSDKBuilderOwnsInputVerification(t *testing.T) {
	for _, change := range []string{"none", testReorg, "mempool spend"} {
		t.Run(change, func(t *testing.T) {
			fixture := newCustomAnchorBuilderFixture(t)
			p, err := LastProof(fixture.request.Inputs[0].ProofFile)
			require.NoError(t, err)
			chain := &bindingChain{
				tip:     p.BlockHeight + 1,
				unspent: true,
				blocks: map[uint32]*wire.MsgBlock{
					p.BlockHeight: bindingBlock(p),
				},
			}
			fixture.client.afterVerify = func() {
				switch change {
				case testReorg:
					chain.blocks[p.BlockHeight].Header.Nonce++
				case "mempool spend":
					chain.unspent = false
				}
			}
			require.NoError(
				t,
				CheckInputsCurrent(t.Context(), chain, 1,
					fixture.request, true),
			)
			require.Zero(t, fixture.client.verifyCalls)
			wallet := sdk.NewWallet(
				fixture.client,
				sdk.NetworkRegtest,
			)
			_, err = wallet.NewCustomAnchorTxBuilder().
				Build(t.Context(), fixture.request)
			require.NoError(t, err)
			require.Equal(t, 1, fixture.client.verifyCalls)
			err = CheckInputsCurrent(
				t.Context(),
				chain,
				1,
				fixture.request,
				true,
			)
			if change == "none" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, swapkit.ErrPending)
			}
			require.Equal(t, 1, fixture.client.verifyCalls)
		})
	}
}
