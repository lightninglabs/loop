package runtime

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/stretchr/testify/require"
)

func TestCachedOutcomeRelocationWithoutPeer(t *testing.T) {
	for _, refund := range []bool{false, true} {
		r, s, p, chain := bindingOutcome(t, refund)
		old := bindingRaw(t, p)
		require.NoError(t, r.verifyOutcome(t.Context(), s, old, refund))
		require.NoError(t, r.put("test/outcome", old))
		chain.blocks[160] = &wire.MsgBlock{
			Header: wire.BlockHeader{Nonce: 1},
		}
		p.BlockHeight = 161
		p.BlockHeader.Nonce = 2
		chain.blocks[161] = bindingBlock(p)
		chain.tip = 162
		fresh := bindingRaw(t, p)
		var cached []byte
		require.NoError(t, r.get("test/outcome", &cached))
		calls := 0
		verify := func(ctx context.Context, raw []byte) error {
			return r.verifyOutcome(ctx, s, raw, refund)
		}
		result, err := resolveOutcomeProof(t.Context(), nil, cached,
			verify, func(context.Context) ([]byte, error) {
				calls++
				return fresh, nil
			})
		require.NoError(t, err)
		require.Equal(t, fresh, result)
		require.Equal(t, 1, calls)
		require.NoError(t, r.get("test/outcome", &cached))
		require.Equal(t, old, cached)

		// Invalid or still-unconfirmed discovery is never accepted.
		_, err = resolveOutcomeProof(t.Context(), nil, cached,
			verify, func(context.Context) ([]byte, error) {
				return old, nil
			})
		require.ErrorIs(t, err, bridge.ErrPending)
	}
}

func TestOutcomeCacheDoesNotSuppressInvalidEvidence(t *testing.T) {
	calls := 0
	verify := func(context.Context, []byte) error {
		return bridge.ErrInvalid
	}
	discover := func(context.Context) ([]byte, error) {
		calls++
		return []byte{2}, nil
	}
	_, err := resolveOutcomeProof(t.Context(), nil, []byte{1},
		verify, discover)
	require.ErrorIs(t, err, bridge.ErrInvalid)
	require.Zero(t, calls)
	_, err = resolveOutcomeProof(t.Context(), []byte{1}, nil,
		verify, discover)
	require.ErrorIs(t, err, bridge.ErrInvalid)
	require.Zero(t, calls)
}
