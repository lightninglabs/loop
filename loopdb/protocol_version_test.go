package loopdb

import (
	"testing"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
)

// TestProtocolVersionSanity tests that protocol versions are sane, meaning
// we always keep our stored protocol version in sync with the RPC protocol
// version except for the unrecorded version.
func TestProtocolVersionSanity(t *testing.T) {
	t.Parallel()

	versions := [...]ProtocolVersion{
		ProtocolVersionLegacy,
		ProtocolVersionMultiLoopOut,
		ProtocolVersionSegwitLoopIn,
		ProtocolVersionPreimagePush,
		ProtocolVersionUserExpiryLoopOut,
		ProtocolVersionHtlcV2,
		ProtocolVersionMultiLoopIn,
		ProtocolVersionLoopOutCancel,
		ProtocolVersionProbe,
	}

	rpcVersions := [...]swapserverrpc.ProtocolVersion{
		swapserverrpc.ProtocolVersion_LEGACY,
		swapserverrpc.ProtocolVersion_MULTI_LOOP_OUT,
		swapserverrpc.ProtocolVersion_NATIVE_SEGWIT_LOOP_IN,
		swapserverrpc.ProtocolVersion_PREIMAGE_PUSH_LOOP_OUT,
		swapserverrpc.ProtocolVersion_USER_EXPIRY_LOOP_OUT,
		swapserverrpc.ProtocolVersion_HTLC_V2,
		swapserverrpc.ProtocolVersion_MULTI_LOOP_IN,
		swapserverrpc.ProtocolVersion_LOOP_OUT_CANCEL,
		swapserverrpc.ProtocolVersion_PROBE,
	}

	require.Equal(t, len(versions), len(rpcVersions))
	for i, version := range versions {
		require.Equal(t, uint32(version), uint32(rpcVersions[i]))
	}

	// Finally test that the current version contants are up to date
	require.Equal(t,
		CurrentInternalProtocolVersion,
		versions[len(versions)-1],
	)

	require.Equal(t,
		uint32(CurrentInternalProtocolVersion),
		uint32(CurrentRPCProtocolVersion),
	)
}
