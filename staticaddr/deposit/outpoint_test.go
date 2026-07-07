package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestCheckDuplicates(t *testing.T) {
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

	err := CheckDuplicates(outpoints)
	require.ErrorContains(t, err, "duplicate outpoint")
	require.ErrorContains(t, err, duplicate.String())
}

func TestCheckDuplicatesNoDuplicate(t *testing.T) {
	outpoints := []wire.OutPoint{{
		Hash:  chainhash.Hash{1},
		Index: 2,
	}, {
		Hash:  chainhash.Hash{3},
		Index: 4,
	}}

	require.NoError(t, CheckDuplicates(outpoints))
}
