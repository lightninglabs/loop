package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalAssetIdentifier(t *testing.T) {
	canonical := strings.Repeat("ab", 32)
	raw, err := canonicalAssetHex(canonical, 32)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	for _, input := range []string{
		strings.ToUpper(canonical), "0x" + canonical, canonical[:62],
	} {
		_, err := canonicalAssetHex(input, 32)
		require.Error(t, err)
	}
}
