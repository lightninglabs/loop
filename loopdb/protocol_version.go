package loopdb

import (
	"math"

	looprpc "github.com/lightninglabs/loop/swapserverrpc"
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

	// ProtocolVersionMultiLoopIn indicates that the client creates a probe
	// invoice so that the server can perform a multi-path probe.
	ProtocolVersionMultiLoopIn ProtocolVersion = 6

	// ProtocolVersionLoopOutCancel indicates that the client supports
	// canceling loop out swaps.
	ProtocolVersionLoopOutCancel = 7

	// ProtocolVerionProbe indicates that the client is able to request
	// the server to perform a probe to test inbound liquidty.
	ProtocolVersionProbe ProtocolVersion = 8

	// The client may ask the server to use a custom routing helper plugin
	// in order to enhance off-chain payments corresponding to a swap.
	ProtocolVersionRoutingPlugin = 9

	// ProtocolVersionHtlcV3 indicates that the client will now use the new
	// HTLC v3 (P2TR) script for swaps.
	ProtocolVersionHtlcV3 = 10

	// ProtocolVersionMuSig2 will enable MuSig2 signature scheme for loops.
	ProtocolVersionMuSig2 ProtocolVersion = 11

	// ProtocolVersionUnrecorded is set for swaps were created before we
	// started saving protocol version with swaps.
	ProtocolVersionUnrecorded ProtocolVersion = math.MaxUint32

	// stableRPCProtocolVersion defines the current stable RPC protocol
	// version.
	stableRPCProtocolVersion = looprpc.ProtocolVersion_MUSIG2

	// experimentalRPCProtocolVersion defines the RPC protocol version that
	// includes all currently experimentally released features.
	experimentalRPCProtocolVersion = stableRPCProtocolVersion
)

var (
	// currentRPCProtocolVersion holds the version of the RPC protocol
	// that the client selected to use for new swaps. Shouldn't be lower
	// than the previous protocol version.
	currentRPCProtocolVersion = stableRPCProtocolVersion
)

// CurrentRPCProtocolVersion returns the RPC protocol version selected to be
// used for new swaps.
func CurrentRPCProtocolVersion() looprpc.ProtocolVersion {
	return currentRPCProtocolVersion
}

// CurrentProtocolVersion returns the internal protocol version selected to be
// used for new swaps.
func CurrentProtocolVersion() ProtocolVersion {
	return ProtocolVersion(currentRPCProtocolVersion)
}

// EnableExperimentalProtocol sets the current protocol version to include all
// experimental features. Do not call this function directly: used in loopd and
// unit tests only.
func EnableExperimentalProtocol() {
	currentRPCProtocolVersion = experimentalRPCProtocolVersion
}

// ResetCurrentProtocolVersion resets the current protocol version to the stable
// protocol. Note: used in integration tests only!
func ResetCurrentProtocolVersion() {
	currentRPCProtocolVersion = stableRPCProtocolVersion
}

// Valid returns true if the value of the ProtocolVersion is valid.
func (p ProtocolVersion) Valid() bool {
	return p <= ProtocolVersion(experimentalRPCProtocolVersion)
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

	case ProtocolVersionLoopOutCancel:
		return "Loop Out Cancel"

	case ProtocolVersionProbe:
		return "Probe"

	case ProtocolVersionRoutingPlugin:
		return "Routing Plugin"

	case ProtocolVersionHtlcV3:
		return "HTLC V3"

	case ProtocolVersionMuSig2:
		return "MuSig2"

	default:
		return "Unknown"
	}
}
