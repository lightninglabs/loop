package version

import (
	"github.com/lightninglabs/loop/swapserverrpc"
)

// AddressProtocolVersion represents the protocol version (declared on rpc
// level) that the client declared to us.
type AddressProtocolVersion uint32

const (
	// ProtocolVersion_V0 indicates that the client is a legacy version
	// that did not report its protocol version.
	ProtocolVersion_V0 AddressProtocolVersion = 0

	// stableRPCProtocolVersion defines the current stable RPC protocol
	// version.
	stableRPCProtocolVersion = swapserverrpc.StaticAddressProtocolVersion_V0
)

var (
	// currentRPCProtocolVersion holds the version of the RPC protocol
	// that the client selected to use for new swaps. Shouldn't be lower
	// than the previous protocol version.
	currentRPCProtocolVersion = stableRPCProtocolVersion
)

// CurrentRPCProtocolVersion returns the RPC protocol version selected to be
// used for new swaps.
func CurrentRPCProtocolVersion() swapserverrpc.StaticAddressProtocolVersion {
	return currentRPCProtocolVersion
}

// Valid returns true if the value of the AddressProtocolVersion is valid.
func (p AddressProtocolVersion) Valid() bool {
	return p <= AddressProtocolVersion(stableRPCProtocolVersion)
}

// String returns the string representation of a protocol version.
func (p AddressProtocolVersion) String() string {
	switch p {
	case ProtocolVersion_V0:
		return "V0"

	default:
		return "Unknown"
	}
}
