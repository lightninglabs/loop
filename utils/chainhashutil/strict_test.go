package chainhashutil

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

// TestNewHashFromStrExact verifies that strict hash parsing rejects any
// non-fully-specified chainhash string.
func TestNewHashFromStrExact(t *testing.T) {
	t.Parallel()

	validHash := strings.Repeat("01", 32)

	testCases := []struct {
		name    string
		hash    string
		wantErr string
	}{
		{
			name:    "valid",
			hash:    validHash,
			wantErr: "",
		},
		{
			name:    "short",
			hash:    validHash[:62],
			wantErr: "invalid hash string length",
		},
		{
			name:    "odd length",
			hash:    validHash[:63],
			wantErr: "invalid hash string length",
		},
		{
			name:    "empty",
			hash:    "",
			wantErr: "invalid hash string length",
		},
		{
			name:    "non hex",
			hash:    strings.Repeat("z", 64),
			wantErr: "invalid byte",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			hash, err := NewHashFromStrExact(testCase.hash)
			if testCase.wantErr != "" {
				require.ErrorContains(t, err, testCase.wantErr)
				require.Equal(t, chainhash.Hash{}, hash)
			} else {
				require.NoError(t, err)
				require.Equal(t, testCase.hash, hash.String())
			}
		})
	}
}
