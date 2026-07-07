package outpoint

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestFirstDuplicate(t *testing.T) {
	duplicate := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 2,
	}

	outpoints := []wire.OutPoint{{
		Hash:  chainhash.Hash{3},
		Index: 4,
	}, duplicate, {
		Hash:  chainhash.Hash{5},
		Index: 6,
	}, duplicate}

	got, ok := FirstDuplicate(outpoints)
	require.True(t, ok)
	require.Equal(t, duplicate, got)
	require.True(t, HasDuplicates(outpoints))
}

func TestFirstDuplicateNoDuplicate(t *testing.T) {
	outpoints := []wire.OutPoint{{
		Hash:  chainhash.Hash{1},
		Index: 2,
	}, {
		Hash:  chainhash.Hash{3},
		Index: 4,
	}}

	got, ok := FirstDuplicate(outpoints)
	require.False(t, ok)
	require.Equal(t, wire.OutPoint{}, got)
	require.False(t, HasDuplicates(outpoints))
}
