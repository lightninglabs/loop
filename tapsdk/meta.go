package tapsdk

// AssetMetaType represents the type of asset metadata.
type AssetMetaType uint8

const (
	// AssetMetaTypeOpaque means the metadata is opaque bytes with no
	// assumed structure.
	AssetMetaTypeOpaque AssetMetaType = 0

	// AssetMetaTypeJSON means the metadata is structured JSON.
	AssetMetaTypeJSON AssetMetaType = 1
)

// FetchAssetMetaRequest specifies how to look up asset metadata. Exactly
// one of the fields must be set.
type FetchAssetMetaRequest struct {
	// AssetRef identifies the asset whose metadata should be fetched.
	AssetRef *AssetRef

	// MetaHash is the 32-byte meta hash.
	MetaHash *Hash
}

// AssetMeta contains the metadata for an asset.
type AssetMeta struct {
	// Data is the raw metadata bytes. Limited to 1 MiB.
	Data []byte

	// Type is the metadata type.
	Type AssetMetaType

	// MetaHash is the hash of the TLV serialization of the meta.
	MetaHash Hash

	// UnknownOddTypes is a map of unknown odd TLV types encountered
	// during decoding.
	UnknownOddTypes map[uint64][]byte

	// DecimalDisplay is the number of decimal places for display.
	DecimalDisplay uint32

	// UniverseCommitments indicates whether the issuer publishes
	// universe supply commitments.
	UniverseCommitments bool

	// CanonicalUniverseURLs are the issuer's canonical universe URLs.
	CanonicalUniverseURLs []string

	// DelegationKey is the public key used to verify universe supply
	// commitment outputs and proofs.
	DelegationKey []byte
}
