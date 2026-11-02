package tapsdk

// ManagedUtxo represents a UTXO managed by the tapd daemon.
type ManagedUtxo struct {
	// OutPoint is the outpoint of the UTXO.
	OutPoint Outpoint

	// AmtSat is the UTXO amount in satoshis.
	AmtSat int64

	// InternalKey is the internal key used for the on-chain output.
	InternalKey PubKey

	// TaprootAssetRoot is the Taproot Asset root commitment hash.
	TaprootAssetRoot Hash

	// MerkleRoot is the Taproot merkle root hash.
	MerkleRoot Hash

	// Assets are the low-level asset records held at this UTXO.
	Assets []*AssetRecord

	// LeaseOwner is the lease owner for this UTXO. Empty if unleased.
	LeaseOwner []byte

	// LeaseExpiryUnix is the unix timestamp when the lease expires.
	// Zero if unleased.
	LeaseExpiryUnix int64
}

// ListUtxosRequest specifies filters for listing managed UTXOs.
type ListUtxosRequest struct {
	// IncludeLeased includes UTXOs that are marked as leased.
	IncludeLeased bool

	// ScriptKeyType filters the assets by script key type.
	ScriptKeyType *ScriptKeyTypeQuery
}

// AssetGroupMember is one protocol-level member returned by tapd's group
// listing RPC. It is not the SDK business Asset type; high-level callers should
// use Wallet.ListAssets, Wallet.ListCollections, or Wallet.ListIssuances.
type AssetGroupMember struct {
	// AssetRef is the SDK identifier for the containing group.
	AssetRef AssetRef

	// IssuanceID is the 32-byte protocol-level identifier of this issuance.
	IssuanceID AssetID

	// Amount is the asset amount.
	Amount uint64

	// LockTime is the optional absolute locktime.
	LockTime int32

	// RelativeLockTime is the optional relative locktime.
	RelativeLockTime int32

	// Tag is the name of the asset.
	Tag string

	// MetaHash is the metadata hash.
	MetaHash Hash

	// Type is the asset type.
	Type AssetType

	// Version is the asset version.
	Version uint8
}

// AssetGroupRecord represents a low-level Taproot Asset group listing from
// tapd. Use Wallet.ListAssets, Wallet.ListCollections, or Wallet.ListIssuances
// for the high-level business entities.
type AssetGroupRecord struct {
	// AssetRef is the SDK identifier for this group. Callers that need
	// the raw group public key can extract it via AssetRef.GroupKey().
	AssetRef AssetRef

	// Members are the concrete issuances or NFT items in this protocol group.
	Members []*AssetGroupMember
}
