package tapsdk

import (
	"context"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// scriptPackage loads the public SDK signing fixture. Only tests know the
// deterministic private key; production always uses the normal LND signer.
func scriptPackage(t *testing.T) *sdk.CustomAnchorTransferPackage {
	t.Helper()
	raw, err := os.ReadFile("testdata/script.package")
	require.NoError(t, err)
	var pkg sdk.CustomAnchorTransferPackage
	require.NoError(t, pkg.UnmarshalBinary(raw))
	// The fixture has two independent key-path inputs. Complete those
	// before exercising the production script signer.
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	result := &pkg
	for i, request := range requests.KeyPath {
		key := bindingKey([]byte{2, 9}[i])
		tweaked := txscript.TweakTaprootPrivKey(*key,
			request.TaprootMerkleRoot)
		signature, err := schnorr.Sign(tweaked, request.Sighash[:])
		require.NoError(t, err)
		result, err = result.ApplyKeyPathSignature(request.ID,
			signature.Serialize())
		require.NoError(t, err)
	}
	packet, err := decodePSBT(result.AnchorPsbt)
	require.NoError(t, err)
	for _, request := range requests.KeyPath {
		complete, err := psbt.MaybeFinalize(packet, int(request.InputIndex))
		require.NoError(t, err)
		require.True(t, complete)
	}
	result.AnchorPsbt, err = encodePSBT(packet)
	require.NoError(t, err)
	result, err = result.Seal()
	require.NoError(t, err)
	return result
}

func TestOutcomeRelocationWithoutPeer(t *testing.T) {
	for _, refund := range []bool{false, true} {
		r, s, p, chain := bindingOutcome(t, refund)
		old := bindingRaw(t, p)
		require.NoError(t, r.verifyOutcome(t.Context(), s, old, refund))
		chain.blocks[160] = &wire.MsgBlock{
			Header: wire.BlockHeader{Nonce: 1},
		}
		p.BlockHeight = 161
		p.BlockHeader.Nonce = 2
		chain.blocks[161] = bindingBlock(p)
		chain.tip = 162
		fresh := bindingRaw(t, p)
		r = &Driver{cfg: r.cfg}
		calls := 0
		verify := func(ctx context.Context, raw []byte) error {
			return r.verifyOutcome(ctx, s, raw, refund)
		}
		result, err := resolveOutcomeProof(t.Context(), nil,
			verify, func(context.Context) ([]byte, error) {
				calls++
				return fresh, nil
			})
		require.NoError(t, err)
		require.Equal(t, fresh, result)
		require.Equal(t, 1, calls)

		// Invalid or still-unconfirmed discovery is never accepted.
		_, err = resolveOutcomeProof(t.Context(), nil,
			verify, func(context.Context) ([]byte, error) {
				return old, nil
			})
		require.ErrorIs(t, err, swapkit.ErrPending)
	}
}

func TestOutcomeDoesNotSuppressInvalidEvidence(t *testing.T) {
	calls := 0
	verify := func(context.Context, []byte) error {
		return swapkit.ErrInvalid
	}
	discover := func(context.Context) ([]byte, error) {
		calls++
		return []byte{2}, nil
	}
	_, err := resolveOutcomeProof(t.Context(), []byte{1},
		verify, discover)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	require.Zero(t, calls)
}
