package tapsdk

// ProofType distinguishes issuance and transfer universe proofs.
type ProofType int

const (
	// ProofTypeUnspecified is the default/zero proof type.
	ProofTypeUnspecified ProofType = 0

	// ProofTypeIssuance limits a query to issuance proofs.
	ProofTypeIssuance ProofType = 1

	// ProofTypeTransfer limits a query to transfer proofs.
	ProofTypeTransfer ProofType = 2
)

// UniverseID identifies a universe by AssetRef plus a proof type.
type UniverseID struct {
	// AssetRef identifies the asset universe to query.
	AssetRef AssetRef

	// ProofType filters the universe by proof type.
	ProofType ProofType
}

// UniverseIDFromRef constructs a UniverseID from an AssetRef and
// proof type. Fungible refs (group key) produce a group-key universe
// ID; collectible refs (asset ID) produce an asset-ID universe ID.
func UniverseIDFromRef(
	ref AssetRef, proofType ProofType) UniverseID {

	return UniverseID{AssetRef: ref, ProofType: proofType}
}

// MerkleSumNode is a node in the Merkle-Sum Sparse Merkle Tree.
type MerkleSumNode struct {
	// RootHash is the MS-SMT root hash.
	RootHash Hash

	// RootSum is the aggregate supply sum of the subtree.
	RootSum int64
}

// UniverseRoot is the root of a single asset universe.
type UniverseRoot struct {
	// ID is the universe identifier.
	ID UniverseID

	// MSSMTRoot is the Merkle-Sum SMT root.
	MSSMTRoot *MerkleSumNode

	// AssetName is the human-readable name of the asset.
	AssetName string

	// AmountsByIssuanceID maps hex-encoded issuance IDs to the number of units
	// minted. For multi-issuance assets this may contain more than one entry.
	AmountsByIssuanceID map[string]uint64
}

// AssetRootRequest configures pagination for AssetRoots queries.
type AssetRootRequest struct {
	// WithAmountsByID includes per-asset-ID amounts for grouped assets.
	WithAmountsByID bool

	// Offset is the page offset.
	Offset int32

	// Limit is the maximum number of roots to return.
	Limit int32

	// Direction is the page sort direction.
	Direction SortDirection
}

// QueryRootResponse is the response for a single-asset root query.
type QueryRootResponse struct {
	// IssuanceRoot is the issuance universe root.
	IssuanceRoot *UniverseRoot

	// TransferRoot is the transfer universe root.
	TransferRoot *UniverseRoot
}

// AssetLeafKey is a key in the universe tree, combining an outpoint and
// script key.
type AssetLeafKey struct {
	// Outpoint identifies the on-chain anchor point.
	Outpoint Outpoint

	// ScriptKey is the asset script key.
	ScriptKey PubKey
}

// AssetLeafKeysRequest filters asset leaf key queries.
type AssetLeafKeysRequest struct {
	// ID identifies which universe to query.
	ID UniverseID

	// Offset is the page offset.
	Offset int32

	// Limit is the maximum number of keys to return.
	Limit int32

	// Direction is the page sort direction.
	Direction SortDirection
}

// AssetLeaf is a leaf in the universe tree.
type AssetLeaf struct {
	// Asset is the low-level asset record associated with this leaf, if
	// present.
	Asset *AssetRecord

	// Proof is the raw issuance or transfer proof bytes.
	Proof []byte
}

// UniverseKey is the full key path into a universe: ID + leaf key.
type UniverseKey struct {
	// ID identifies the universe.
	ID UniverseID

	// LeafKey identifies the specific leaf.
	LeafKey AssetLeafKey
}

// AssetProofResponse is the response from a universe proof query.
type AssetProofResponse struct {
	// Key is the original request key.
	Key UniverseKey

	// UniverseRoot is the root that includes this proof.
	UniverseRoot *UniverseRoot

	// UniverseInclusionProof is the raw MS-SMT inclusion proof.
	UniverseInclusionProof []byte

	// AssetLeaf is the leaf containing the asset and proof.
	AssetLeaf *AssetLeaf

	// MultiverseRoot is the root of the wider multiverse tree.
	MultiverseRoot *MerkleSumNode

	// MultiverseInclusionProof is the multiverse inclusion proof.
	MultiverseInclusionProof []byte
}

// UniverseRoots groups the issuance and transfer roots for one AssetRef.
type UniverseRoots struct {
	// AssetRef identifies the asset universe.
	AssetRef AssetRef

	// IssuanceRoot is the issuance proof root, if known locally.
	IssuanceRoot *UniverseRoot

	// TransferRoot is the transfer proof root, if known locally.
	TransferRoot *UniverseRoot
}

// HasRoots reports whether at least one universe root is present.
func (r *UniverseRoots) HasRoots() bool {
	return r != nil && (r.IssuanceRoot != nil || r.TransferRoot != nil)
}

// UniverseProof is a high-level universe proof entry for one AssetRef.
type UniverseProof struct {
	// AssetRef identifies the asset universe that contains this proof.
	AssetRef AssetRef

	// ProofType identifies whether this is an issuance or transfer proof.
	ProofType ProofType

	// LeafKey identifies the concrete proof leaf in the universe tree.
	LeafKey AssetLeafKey

	// Proof is the raw issuance or transfer proof bytes.
	Proof []byte

	// Asset is the decoded low-level asset record, if tapd returned one.
	Asset *AssetRecord

	// UniverseRoot is the root that includes this proof.
	UniverseRoot *UniverseRoot

	// UniverseInclusionProof is the raw MS-SMT inclusion proof.
	UniverseInclusionProof []byte

	// MultiverseRoot is the root of the wider multiverse tree.
	MultiverseRoot *MerkleSumNode

	// MultiverseInclusionProof is the multiverse inclusion proof.
	MultiverseInclusionProof []byte
}

// UniverseStats contains aggregate universe statistics.
type UniverseStats struct {
	// NumTotalAssets is the total number of known assets.
	NumTotalAssets int64

	// NumTotalGroups is the total number of known groups.
	NumTotalGroups int64

	// NumTotalSyncs is the total number of universe syncs.
	NumTotalSyncs int64

	// NumTotalProofs is the total number of stored proofs.
	NumTotalProofs int64
}

// AssetQuerySort controls the sort field for asset statistics queries.
type AssetQuerySort int

const (
	// SortByNone applies no sorting.
	SortByNone AssetQuerySort = 0

	// SortByAssetName sorts by asset name.
	SortByAssetName AssetQuerySort = 1

	// SortByIssuanceID sorts by concrete issuance asset ID.
	SortByIssuanceID AssetQuerySort = 2

	// SortByAssetType sorts by asset type.
	SortByAssetType AssetQuerySort = 3

	// SortByTotalSyncs sorts by total sync count.
	SortByTotalSyncs AssetQuerySort = 4

	// SortByTotalProofs sorts by total proof count.
	SortByTotalProofs AssetQuerySort = 5

	// SortByGenesisHeight sorts by genesis block height.
	SortByGenesisHeight AssetQuerySort = 6

	// SortByTotalSupply sorts by total asset supply.
	SortByTotalSupply AssetQuerySort = 7
)

// AssetTypeFilter controls asset type filtering in stats queries.
type AssetTypeFilter int

const (
	// FilterAssetNone returns all asset types.
	FilterAssetNone AssetTypeFilter = 0

	// FilterAssetNormal returns only normal (fungible) assets.
	FilterAssetNormal AssetTypeFilter = 1

	// FilterAssetCollectible returns only collectible assets.
	FilterAssetCollectible AssetTypeFilter = 2
)

// AssetStatsQuery configures an asset statistics query.
type AssetStatsQuery struct {
	// AssetNameFilter filters by asset name substring.
	AssetNameFilter string

	// AssetRefFilter filters by a specific asset.
	AssetRefFilter *AssetRef

	// AssetTypeFilter restricts results to a specific asset type.
	AssetTypeFilter AssetTypeFilter

	// SortBy controls the sort field.
	SortBy AssetQuerySort

	// Offset is the pagination offset.
	Offset int32

	// Limit is the pagination page size.
	Limit int32

	// Direction is the sort direction.
	Direction SortDirection
}

// AssetStatsAsset is a per-asset statistics record.
type AssetStatsAsset struct {
	// AssetRef is the SDK's user-facing identifier for the asset.
	AssetRef AssetRef

	// IssuanceID is the 32-byte protocol-level identifier for the specific
	// issuance/tranche represented by this stats record.
	IssuanceID AssetID

	// GenesisPoint is the genesis outpoint string.
	GenesisPoint string

	// TotalSupply is the total units minted.
	TotalSupply int64

	// AssetName is the human-readable name.
	AssetName string

	// AssetType is the type (normal or collectible).
	AssetType AssetType

	// GenesisHeight is the block height at creation.
	GenesisHeight int32

	// GenesisTimestamp is the block timestamp (Unix seconds).
	GenesisTimestamp int64

	// AnchorPoint is the current anchor point string.
	AnchorPoint string

	// DecimalDisplay is the number of decimal places.
	DecimalDisplay uint32
}

// AssetStatsSnapshot is a statistics snapshot for one asset or group.
type AssetStatsSnapshot struct {
	// AssetRef is the SDK's user-facing identifier for the asset.
	AssetRef AssetRef

	// GroupKey is the group key, if the asset belongs to a group.
	GroupKey *PubKey

	// GroupSupply is the total group supply (zero for ungrouped assets).
	GroupSupply int64

	// GroupAnchor is the group anchor asset stats, if grouped.
	GroupAnchor *AssetStatsAsset

	// Asset is the single asset stats, if not grouped.
	Asset *AssetStatsAsset

	// TotalSyncs is the sync count.
	TotalSyncs int64

	// TotalProofs is the proof count.
	TotalProofs int64
}

// QueryEventsRequest configures a time-range event query.
type QueryEventsRequest struct {
	// StartTimestamp is the start of the range (Unix seconds).
	StartTimestamp int64

	// EndTimestamp is the end of the range (Unix seconds).
	EndTimestamp int64
}

// GroupedUniverseEvents are daily aggregated event counts.
type GroupedUniverseEvents struct {
	// Date is the day formatted as YYYY-MM-DD.
	Date string

	// SyncEvents is the number of sync events on this day.
	SyncEvents uint64

	// NewProofEvents is the number of new proof events on this day.
	NewProofEvents uint64
}

// FederationServer is a universe federation peer.
type FederationServer struct {
	// Host is the address of the federation server.
	Host string

	// ID is the numeric server identifier.
	ID int32
}

// SyncTarget identifies an asset to sync.
type SyncTarget struct {
	// ID is the universe ID to sync.
	ID UniverseID
}

// UniverseSyncMode selects the sync scope.
type UniverseSyncMode int

const (
	// SyncIssuanceOnly syncs only issuance proofs.
	SyncIssuanceOnly UniverseSyncMode = 0

	// SyncFull syncs all proofs including transfers.
	SyncFull UniverseSyncMode = 1
)

// SyncRequest configures a universe sync operation.
type SyncRequest struct {
	// UniverseHost is the remote server to sync with.
	UniverseHost string

	// SyncMode controls the sync scope.
	SyncMode UniverseSyncMode

	// SyncTargets limits the sync to specific assets. An empty list
	// means sync everything.
	SyncTargets []SyncTarget
}

// SyncedUniverse is the result of syncing a single universe.
type SyncedUniverse struct {
	// OldAssetRoot is the root before the sync.
	OldAssetRoot *UniverseRoot

	// NewAssetRoot is the root after the sync.
	NewAssetRoot *UniverseRoot

	// NewAssetLeaves are the newly synced leaves.
	NewAssetLeaves []AssetLeaf
}

// UniverseSyncResult is the high-level sync result for one AssetRef.
type UniverseSyncResult struct {
	// AssetRef identifies the synced asset universe.
	AssetRef AssetRef

	// Issuance is the issuance-universe diff, if one changed.
	Issuance *SyncedUniverse

	// Transfer is the transfer-universe diff, if one changed.
	Transfer *SyncedUniverse
}

// GlobalFederationSyncConfig is a per-proof-type federation sync policy.
type GlobalFederationSyncConfig struct {
	// ProofType is the universe proof type this config applies to.
	ProofType ProofType

	// AllowSyncInsert enables federation insert for this type.
	AllowSyncInsert bool

	// AllowSyncExport enables federation export for this type.
	AllowSyncExport bool
}

// AssetFederationSyncConfig is a per-asset federation sync policy.
type AssetFederationSyncConfig struct {
	// ID is the universe ID this config applies to.
	ID UniverseID

	// AllowSyncInsert enables federation insert for this asset.
	AllowSyncInsert bool

	// AllowSyncExport enables federation export for this asset.
	AllowSyncExport bool
}

// FederationSyncConfig combines global and per-asset sync configs.
type FederationSyncConfig struct {
	// GlobalSyncConfigs are the per-proof-type configs.
	GlobalSyncConfigs []GlobalFederationSyncConfig

	// AssetSyncConfigs are the per-asset configs.
	AssetSyncConfigs []AssetFederationSyncConfig
}

// UniverseInfo contains basic server info.
type UniverseInfo struct {
	// RuntimeID is a per-restart pseudo-random identifier.
	RuntimeID int64
}
