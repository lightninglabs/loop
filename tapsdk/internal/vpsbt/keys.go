package vpsbt

// PSBT key prefixes for virtual transaction encoding.
var (
	// Global keys.
	keyGlobalIsVirtualTx    = []byte{0x70}
	keyGlobalChainParamsHRP = []byte{0x71}
	keyGlobalPsbtVersion    = []byte{0x72}

	// Input keys.
	keyInputPrevID = []byte{0x70}

	// Output keys.
	keyOutputType                          = []byte{0x70}
	keyOutputIsInteractive                 = []byte{0x71}
	keyOutputAnchorOutputIndex             = []byte{0x72}
	keyOutputAnchorOutputInternalKey       = []byte{0x73}
	keyOutputAnchorOutputBip32Derivation   = []byte{0x74}
	keyOutputAnchorOutputTrBip32Derivation = []byte{0x75}
	keyOutputAssetVersion                  = []byte{0x79}
	keyOutputLockTime                      = []byte{0x7c}
	keyOutputRelativeLockTime              = []byte{0x7d}
	keyOutputAltLeaves                     = []byte{0x7e}
)

// VPacket versions.
const (
	VPacketVersionV0 uint8 = 0
	VPacketVersionV1 uint8 = 1
)

// VOutput types.
const (
	VOutputTypeSimple    uint8 = 0
	VOutputTypeSplitRoot uint8 = 1
)

// Asset versions.
const (
	AssetVersionV0 uint8 = 0
	AssetVersionV1 uint8 = 1
)

// Network HRPs for chain params.
const (
	// MainnetHRP is the HRP for mainnet.
	MainnetHRP = "tapbc"

	// TestnetHRP is the HRP for testnet.
	TestnetHRP = "taptb"

	// Testnet4HRP is the HRP for testnet4.
	Testnet4HRP = "taptb"

	// RegTestHRP is the HRP for regtest.
	RegTestHRP = "taprt"

	// SigNetHRP is the HRP for "the" signet.
	SigNetHRP = "taptb"

	// SimNetHRP is the HRP for simnet.
	SimNetHRP = "tapsb"
)

// BIP-0043 purpose for key derivation (same as LND).
const (
	BIP0043Purpose         = 1017
	HardenedKeyStart       = 0x80000000
	TaprootAssetsKeyFamily = 212
)
