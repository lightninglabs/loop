package loopdb

import (
	"testing"

	"github.com/lightninglabs/loop/server/serverrpc"
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
	}

	rpcVersions := [...]serverrpc.ProtocolVersion{
		serverrpc.ProtocolVersion_LEGACY,
		serverrpc.ProtocolVersion_MULTI_LOOP_OUT,
		serverrpc.ProtocolVersion_NATIVE_SEGWIT_LOOP_IN,
		serverrpc.ProtocolVersion_PREIMAGE_PUSH_LOOP_OUT,
		serverrpc.ProtocolVersion_USER_EXPIRY_LOOP_OUT,
		serverrpc.ProtocolVersion_HTLC_V2,
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
