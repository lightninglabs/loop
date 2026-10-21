package tapsdk

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

type changingProofSDK struct {
	sdk.Client

	change func()
	calls  int
}

func (s *changingProofSDK) VerifyProof(context.Context,
	[]byte) (*sdk.VerifyProofResponse, error) {

	s.calls++
	s.change()
	return &sdk.VerifyProofResponse{Valid: true}, nil
}

// TestProofFreshnessAfterSDKVerification changes chain state while tapd is
// verifying the proof. Validation before that RPC cannot authorize a stale
// confirmation or an output that was spent before verification completed.
func TestProofFreshnessAfterSDKVerification(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*bindingChain)
	}{
		{
			name: "reorg",
			change: func(c *bindingChain) {
				c.blocks[100].Header.Nonce++
			},
		},
		{
			name: "lost confirmation depth",
			change: func(c *bindingChain) {
				c.tip = 100
			},
		},
		{
			name: "spent output",
			change: func(c *bindingChain) {
				c.unspent = false
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := bindingProof(t, bindingGenesis(),
				bindingKey(2).PubKey(), bindingKey(3).PubKey(),
				nil, bindingGenesis().FirstPrevOut, 100, false)
			chain := &bindingChain{
				tip: 101, unspent: true,
				blocks: map[uint32]*wire.MsgBlock{
					100: bindingBlock(p),
				},
			}
			client := &changingProofSDK{change: func() {
				test.change(chain)
			}}
			driver := &Driver{cfg: DriverConfig{
				SDK: client, Chain: chain, MinConfs: 2,
			}}
			verified, err := driver.verifyProof(t.Context(),
				bindingRaw(t, p), bindingPub(11), 1000, true)
			require.ErrorIs(t, err, swapkit.ErrPending)
			require.Nil(t, verified)
			require.Equal(t, 1, client.calls)
		})
	}
}

// nativeSignedFixture completes the public package using the existing LND
// session fixture. Both packages retain the same committed transaction.
func nativeSignedFixture(t *testing.T) (*sdk.CustomAnchorTransferPackage,
	*sdk.CustomAnchorTransferPackage) {

	t.Helper()
	approved := signingPackage(t)
	client, err := NewSigningRound(t.Context(), signingWallet(t, 2),
		keychain.KeyLocator{}, approved)
	require.NoError(t, err)
	defer client.Close(t.Context())
	server, err := NewSigningRound(t.Context(), signingWallet(t, 3),
		keychain.KeyLocator{}, approved)
	require.NoError(t, err)
	defer server.Close(t.Context())
	partial, err := server.Partial(t.Context(), client.Nonce())
	require.NoError(t, err)
	signed, err := client.Finalize(t.Context(), server.Nonce(), partial)
	require.NoError(t, err)
	finalized, err := approved.WithFinalAnchorPSBT(
		finishSigningFixture(t, signed),
	)
	require.NoError(t, err)
	return approved, finalized
}

// TestNativeFinalPackageBindsApprovedPolicy checks the claim-acceptance helper
// used by VerifyClaim. A valid final witness and matching unsigned transaction
// do not authorize replacing the approved signing policy or package metadata.
func TestNativeFinalPackageBindsApprovedPolicy(t *testing.T) {
	approved, finalized := nativeSignedFixture(t)
	raw, err := finalized.MarshalBinary()
	require.NoError(t, err)
	bound, err := bindFinalPackage(approved, raw)
	require.NoError(t, err)
	require.Equal(t, finalized.PackageDigest, bound.PackageDigest)
	require.Equal(t, approved.CommittedPackageDigest,
		bound.CommittedPackageDigest)
	require.NotEqual(t, approved.PackageDigest, bound.PackageDigest)

	for _, test := range []struct {
		name   string
		change func(*sdk.CustomAnchorTransferPackage)
	}{
		{
			name: "publish metadata",
			change: func(p *sdk.CustomAnchorTransferPackage) {
				p.Publish.Label += " changed"
			},
		},
		{
			name: "signing session context",
			change: func(p *sdk.CustomAnchorTransferPackage) {
				for i := range p.SigningPlans {
					plan := &p.SigningPlans[i]
					if plan.MuSig2 != nil {
						plan.MuSig2.SessionContext = []byte(
							"another approved operation",
						)
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := finalized.Clone()
			test.change(changed)
			changed, err := changed.Seal()
			require.NoError(t, err)
			require.Equal(t, approved.UnsignedTxDigest,
				changed.UnsignedTxDigest)
			require.NotEqual(t, approved.CommittedPackageDigest,
				changed.CommittedPackageDigest)
			// The substitute is internally valid, including its final
			// witnesses. Only binding to the approved policy rejects it.
			_, err = changed.WithFinalAnchorPSBT(changed.AnchorPsbt)
			require.NoError(t, err)
			raw, err := changed.MarshalBinary()
			require.NoError(t, err)
			bound, err := bindFinalPackage(approved, raw)
			require.Error(t, err)
			require.Nil(t, bound)
		})
	}
}

// TestNativeClaimPackageRequiresFinalWitness prevents validated SDK package
// decoding from being mistaken for full witness validation at claim acceptance.
func TestNativeClaimPackageRequiresFinalWitness(t *testing.T) {
	approved, finalized := nativeSignedFixture(t)
	for _, name := range []string{"unsigned", "partial", "invalid witness"} {
		t.Run(name, func(t *testing.T) {
			proposed := approved.Clone()
			switch name {
			case "unsigned":
				proposed.AnchorPsbt = proposed.CommittedAnchorPsbt
			case "invalid witness":
				proposed = finalized.Clone()
				packet, err := decodePSBT(proposed.AnchorPsbt)
				require.NoError(t, err)
				var witness bytes.Buffer
				require.NoError(t, psbt.WriteTxWitness(
					&witness, [][]byte{{1}},
				))
				packet.Inputs[0].FinalScriptWitness = witness.Bytes()
				proposed.AnchorPsbt, err = encodePSBT(packet)
				require.NoError(t, err)
			}
			proposed, err := proposed.Seal()
			require.NoError(t, err)
			raw, err := proposed.MarshalBinary()
			require.NoError(t, err)
			var decoded sdk.CustomAnchorTransferPackage
			require.NoError(t, decoded.UnmarshalBinary(raw))
			require.Equal(t, approved.CommittedPackageDigest,
				decoded.CommittedPackageDigest)
			bound, err := bindFinalPackage(approved, raw)
			require.Error(t, err)
			require.Nil(t, bound)
		})
	}
}
