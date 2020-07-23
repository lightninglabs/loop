package loopdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProtocolVersionMarshalUnMarshal tests that marshalling and unmarshalling
// looprpc.ProtocolVersion works correctly.
func TestProtocolVersionMarshalUnMarshal(t *testing.T) {
	t.Parallel()

	testVersions := [...]ProtocolVersion{
		ProtocolVersionLegacy,
		ProtocolVersionMultiLoopOut,
		ProtocolVersionSegwitLoopIn,
		ProtocolVersionPreimagePush,
		ProtocolVersionUserExpiryLoopOut,
	}

	bogusVersion := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	invalidSlice := []byte{0xFF, 0xFF, 0xFF}

	for i := 0; i < len(testVersions); i++ {
		testVersion := testVersions[i]

		// Test that unmarshal(marshal(v)) == v.
		version, err := UnmarshalProtocolVersion(
			MarshalProtocolVersion(testVersion),
		)
		require.NoError(t, err)
		require.Equal(t, testVersion, version)

		// Test that unmarshalling a nil slice returns the default
		// version along with no error.
		version, err = UnmarshalProtocolVersion(nil)
		require.NoError(t, err)
		require.Equal(t, ProtocolVersionUnrecorded, version)

		// Test that unmarshalling an unknown version returns the
		// default version along with an error.
		version, err = UnmarshalProtocolVersion(bogusVersion)
		require.Error(t, err, "expected invalid version")
		require.Equal(t, ProtocolVersionUnrecorded, version)

		// Test that unmarshalling an invalid slice returns the
		// default version along with an error.
		version, err = UnmarshalProtocolVersion(invalidSlice)
		require.Error(t, err, "expected invalid size")
		require.Equal(t, ProtocolVersionUnrecorded, version)
	}
}
