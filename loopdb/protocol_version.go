package loopdb

import (
	"math"

	"github.com/lightninglabs/loop/looprpc"
)

// ProtocolVersion represents the protocol version (declared on rpc level) that
// the client declared to us.
type ProtocolVersion uint32

const (
	// ProtocolVersionLegacy indicates that the client is a legacy version
	// that did not report its protocol version.
	ProtocolVersionLegacy ProtocolVersion = 0

	// ProtocolVersionMultiLoopOut indicates that the client supports multi
	// loop out.
	ProtocolVersionMultiLoopOut ProtocolVersion = 1

	// ProtocolVersionSegwitLoopIn indicates that the client supports segwit
	// loop in.
	ProtocolVersionSegwitLoopIn ProtocolVersion = 2

	// ProtocolVersionPreimagePush indicates that the client will push loop
	// out preimages to the sever to speed up claim.
	ProtocolVersionPreimagePush ProtocolVersion = 3

	// ProtocolVersionUserExpiryLoopOut indicates that the client will
	// propose a cltv expiry height for loop out.
	ProtocolVersionUserExpiryLoopOut ProtocolVersion = 4

	// ProtocolVersionHtlcV2 indicates that the client will use the new
	// HTLC v2 scrips for swaps.
	ProtocolVersionHtlcV2 ProtocolVersion = 5

	// ProtocolVersionUnrecorded is set for swaps were created before we
	// started saving protocol version with swaps.
	ProtocolVersionUnrecorded ProtocolVersion = math.MaxUint32

	// CurrentRpcProtocolVersion defines the version of the RPC protocol
	// that is currently supported by the loop client.
	CurrentRPCProtocolVersion = looprpc.ProtocolVersion_HTLC_V2

	// CurrentInteranlProtocolVersionInternal defines the RPC current
	// protocol in the internal representation.
	CurrentInternalProtocolVersion = ProtocolVersion(CurrentRPCProtocolVersion)
)

// Valid returns true if the value of the ProtocolVersion is valid.
func (p ProtocolVersion) Valid() bool {
	return p <= CurrentInternalProtocolVersion
}

// String returns the string representation of a protocol version.
func (p ProtocolVersion) String() string {
	switch p {
	case ProtocolVersionUnrecorded:
		return "Unrecorded"

	case ProtocolVersionLegacy:
		return "Legacy"

	case ProtocolVersionMultiLoopOut:
		return "Multi Loop Out"

	case ProtocolVersionSegwitLoopIn:
		return "Segwit Loop In"

	case ProtocolVersionPreimagePush:
		return "Preimage Push"

	case ProtocolVersionUserExpiryLoopOut:
		return "User Expiry Loop Out"

	case ProtocolVersionHtlcV2:
		return "HTLC V2"

	default:
		return "Unknown"
	}
}
