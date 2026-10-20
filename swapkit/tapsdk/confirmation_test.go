package tapsdk

import (
	"context"
	"os"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// scriptPackage loads the public SDK signing fixture. Only tests know the
// deterministic private key; production always uses the normal LND signer.
func scriptPackage(t *testing.T) transaction {
	t.Helper()
	raw, err := os.ReadFile("testdata/script.package")
	require.NoError(t, err)
	var pkg sdk.CustomAnchorTransferPackage
	require.NoError(t, pkg.UnmarshalBinary(raw))
	return transaction{Package: &pkg}
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
