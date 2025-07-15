package deposit

// AssetDepositProtocolVersion represents the protocol version for asset
// deposits.
type AssetDepositProtocolVersion uint32

const (
	// ProtocolVersion_V0 indicates that the client is a legacy version
	// that did not report its protocol version.
	ProtocolVersion_V0 AssetDepositProtocolVersion = 0

	// stableProtocolVersion defines the current stable RPC protocol
	// version.
	stableProtocolVersion = ProtocolVersion_V0
)

var (
	// currentRPCProtocolVersion holds the version of the RPC protocol
	// that the client selected to use for new swaps. Shouldn't be lower
	// than the previous protocol version.
	currentRPCProtocolVersion = stableProtocolVersion
)

// CurrentRPCProtocolVersion returns the RPC protocol version selected to be
// used for new swaps.
func CurrentProtocolVersion() AssetDepositProtocolVersion {
	return currentRPCProtocolVersion
}

// Valid returns true if the value of the AddressProtocolVersion is valid.
func (p AssetDepositProtocolVersion) Valid() bool {
	return p <= stableProtocolVersion
}

// String returns the string representation of a protocol version.
func (p AssetDepositProtocolVersion) String() string {
	switch p {
	case ProtocolVersion_V0:
		return "ASSET_DEPOSIT_V0"

	default:
		return "Unknown"
	}
}
