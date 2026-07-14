package swap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStaticAddressKeyFamiliesAreDisjoint documents the key-family split used
// by static-address backups and HTLC, receive and change key derivation.
func TestStaticAddressKeyFamiliesAreDisjoint(t *testing.T) {
	families := map[int32]string{
		KeyFamily:                    "swap htlc",
		StaticAddressKeyFamily:       "legacy static address and htlc",
		StaticMultiAddressKeyFamily:  "multi-address receive",
		StaticAddressChangeKeyFamily: "static-address change",
	}

	require.Len(t, families, 4)
	require.EqualValues(t, 99, KeyFamily)
	require.EqualValues(t, 42060, StaticAddressKeyFamily)
	require.EqualValues(t, 42061, StaticMultiAddressKeyFamily)
	require.EqualValues(t, 42062, StaticAddressChangeKeyFamily)
}
