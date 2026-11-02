package tapsdk

import "context"

// The mint entities in this file include the high-level issuer specs and the
// low-level batch minting types exposed by MintClient.

// ExternalKey describes an externally managed BIP-86 key derivation.
type ExternalKey struct {
	// XPub is the account-level extended public key.
	XPub string

	// MasterFingerprint identifies the master key that derived XPub.
	MasterFingerprint [4]byte

	// DerivationPath is the full child derivation path.
	DerivationPath string
}

// IssuanceOperation identifies the high-level issuer action that needs an
// external signature.
type IssuanceOperation uint8

const (
	// IssuanceOperationUnknown means the signing operation was not classified.
	IssuanceOperationUnknown IssuanceOperation = 0

	// IssuanceOperationCreateAsset creates the first issuance of a fungible
	// Asset.
	IssuanceOperationCreateAsset IssuanceOperation = 1

	// IssuanceOperationIssueAsset creates another Issuance for an existing
	// fungible Asset.
	IssuanceOperationIssueAsset IssuanceOperation = 2

	// IssuanceOperationCreateCollection creates the first item in a new
	// Collection.
	IssuanceOperationCreateCollection IssuanceOperation = 3

	// IssuanceOperationMintCollectionItem creates another item in an existing
	// Collection.
	IssuanceOperationMintCollectionItem IssuanceOperation = 4
)

// String returns the stable name for an issuance operation.
func (o IssuanceOperation) String() string {
	switch o {
	case IssuanceOperationCreateAsset:
		return "create_asset"
	case IssuanceOperationIssueAsset:
		return "issue_asset"
	case IssuanceOperationCreateCollection:
		return "create_collection"
	case IssuanceOperationMintCollectionItem:
		return "mint_collection_item"
	default:
		return unknownString
	}
}

// IssuanceSigningRequest describes the SDK-level issuance authorization that
// needs an external Taproot signature.
type IssuanceSigningRequest struct {
	// Operation is the high-level issuer action being authorized.
	Operation IssuanceOperation

	// AssetRef is the stable SDK identifier for the Asset or Collection. For a
	// first issuance this is inferred from the funded signing payload.
	AssetRef AssetRef

	// Name is the Asset or Collection item name.
	Name string

	// AssetType is the kind of asset being issued.
	AssetType AssetType

	// Amount is the number of units this signing request authorizes.
	Amount uint64

	// ScriptKey is the asset-level spending key for the issued units.
	ScriptKey *ScriptKey

	// ExternalKey is the external key descriptor that should sign.
	ExternalKey ExternalKey

	// AnchorGenesis is the concrete genesis information committed by the funded
	// mint batch.
	AnchorGenesis *GenesisInfo

	// VirtualPSBT is the base64 PSBT that must be signed externally.
	VirtualPSBT string

	// VirtualTx is the decoded virtual transaction metadata for review.
	VirtualTx *GroupVirtualTx
}

// SignedIssuance is the external signature result for one issuance signing
// request.
type SignedIssuance struct {
	// VirtualPSBT is the signed base64 PSBT returned by the external signer.
	VirtualPSBT string
}

// ExternalIssuanceSigner signs issuance authorization payloads with an
// externally managed key.
type ExternalIssuanceSigner interface {
	// SignIssuance reviews and signs one issuance authorization request.
	SignIssuance(context.Context, IssuanceSigningRequest) (
		SignedIssuance, error,
	)
}

// PendingMintAsset represents an asset staged in a minting batch.
type PendingMintAsset struct {
	// AssetVersion is the asset encoding version.
	AssetVersion AssetVersion

	// AssetType is the type of asset to mint.
	AssetType AssetType

	// Name is the asset tag.
	Name string

	// AssetMeta is the optional metadata committed to the genesis.
	AssetMeta *AssetMeta

	// Amount is the number of units to mint.
	Amount uint64

	// NewGroupedAsset creates a new asset group for future issuance.
	NewGroupedAsset bool

	// GroupKey joins an existing group by explicit key.
	GroupKey *PubKey

	// GroupAnchor joins the group created by another asset in the batch.
	GroupAnchor string

	// GroupInternalKey is the internal key for a new group.
	GroupInternalKey *KeyDescriptor

	// GroupTapscriptRoot is the optional tapscript root for a new group.
	GroupTapscriptRoot []byte

	// ScriptKey is the optional custom script key for the asset.
	ScriptKey *ScriptKey
}

// BatchState is the lifecycle state of a minting batch.
type BatchState uint8

const (
	// BatchStateUnknown is an unknown batch state.
	BatchStateUnknown BatchState = 0

	// BatchStatePending means the batch is still collecting assets.
	BatchStatePending BatchState = 1

	// BatchStateFrozen means the batch is sealed and no longer mutable.
	BatchStateFrozen BatchState = 2

	// BatchStateCommitted means the genesis transaction was built.
	BatchStateCommitted BatchState = 3

	// BatchStateBroadcast means the genesis transaction was broadcast.
	BatchStateBroadcast BatchState = 4

	// BatchStateConfirmed means the genesis transaction confirmed.
	BatchStateConfirmed BatchState = 5

	// BatchStateFinalized means the batch completed fully.
	BatchStateFinalized BatchState = 6

	// BatchStateSeedlingCancelled means an unfunded batch was cancelled.
	BatchStateSeedlingCancelled BatchState = 7

	// BatchStateSproutCancelled means a funded batch was cancelled.
	BatchStateSproutCancelled BatchState = 8
)

// String returns the stable name for a mint batch state.
func (s BatchState) String() string {
	switch s {
	case BatchStateUnknown:
		return unknownString
	case BatchStatePending:
		return "pending"
	case BatchStateFrozen:
		return "frozen"
	case BatchStateCommitted:
		return "committed"
	case BatchStateBroadcast:
		return "broadcast"
	case BatchStateConfirmed:
		return "confirmed"
	case BatchStateFinalized:
		return "finalized"
	case BatchStateSeedlingCancelled:
		return "seedling_cancelled"
	case BatchStateSproutCancelled:
		return "sprout_cancelled"
	default:
		return "unrecognized"
	}
}

// TapLeaf represents a single leaf in a tapscript tree.
type TapLeaf struct {
	// Script is the tapscript leaf program.
	Script []byte
}

// TapscriptFullTree describes a full ordered tapscript tree.
type TapscriptFullTree struct {
	// Leaves is the ordered list of tapscript leaves.
	Leaves []TapLeaf
}

// TapBranch describes a tapscript tree by its root children.
type TapBranch struct {
	// LeftTapHash is the tap hash of the left child.
	LeftTapHash Hash

	// RightTapHash is the tap hash of the right child.
	RightTapHash Hash
}

// BatchSibling selects the tapscript sibling used for a mint batch.
type BatchSibling struct {
	// FullTree provides the full tapscript tree.
	FullTree *TapscriptFullTree

	// Branch provides only the root branch hashes.
	Branch *TapBranch
}

// GroupWitness authorizes a pending asset to join an asset group.
type GroupWitness struct {
	// GenesisID identifies the pending asset to authorize.
	GenesisID AssetID

	// Witness is the serialized witness stack.
	Witness [][]byte
}

// GenesisInfo describes a concrete issuance genesis record used in mint
// responses.
type GenesisInfo struct {
	// GenesisPoint is the outpoint that created the asset.
	GenesisPoint string

	// Name is the asset name.
	Name string

	// MetaHash is the metadata hash committed into genesis.
	MetaHash Hash

	// IssuanceID is the protocol-level identifier for this concrete
	// issuance/tranche.
	IssuanceID AssetID

	// AssetType is the genesis asset type.
	AssetType AssetType

	// OutputIndex is the anchor output index in the genesis transaction.
	OutputIndex uint32
}

// GroupKeyRequest describes the group membership material for an unsealed
// mint asset.
type GroupKeyRequest struct {
	// RawKey is the group internal key when locally managed.
	RawKey *KeyDescriptor

	// AnchorGenesis is the genesis of the asset anchoring the group.
	AnchorGenesis *GenesisInfo

	// TapscriptRoot is the optional tapscript root used in the group key.
	TapscriptRoot []byte

	// NewAsset is the serialized asset requesting group membership.
	NewAsset []byte

	// ExternalKey is the external key used for group signing, if any.
	ExternalKey *ExternalKey
}

// TxOut represents a transaction output used in a group virtual tx.
type TxOut struct {
	// Value is the amount of the output.
	Value int64

	// PkScript is the output script.
	PkScript []byte
}

// GroupVirtualTx describes the virtual transaction used for group witness
// construction.
type GroupVirtualTx struct {
	// Transaction is the serialized virtual transaction.
	Transaction []byte

	// PrevOut is the output being spent in the virtual transaction.
	PrevOut *TxOut

	// GenesisID is the grouped asset's asset ID.
	GenesisID AssetID

	// TweakedKey is the tweaked group key for the request.
	TweakedKey *PubKey
}

// UnsealedMintAsset contains the extra verbose details for a batch asset.
type UnsealedMintAsset struct {
	// Asset is the pending asset.
	Asset *PendingMintAsset

	// GroupKeyRequest is the request used to derive the group witness.
	GroupKeyRequest *GroupKeyRequest

	// GroupVirtualTx is the virtual transaction for witness creation.
	GroupVirtualTx *GroupVirtualTx

	// GroupVirtualPSBT is the serialized PSBT form of GroupVirtualTx.
	GroupVirtualPSBT string
}

// MintingBatch is the daemon's view of a minting batch.
type MintingBatch struct {
	// BatchKey uniquely identifies the batch.
	BatchKey PubKey

	// BatchTxid is the finalized genesis transaction ID, if any.
	BatchTxid string

	// State is the current lifecycle state.
	State BatchState

	// Assets are the assets currently staged in the batch.
	Assets []PendingMintAsset

	// CreatedAt is the batch creation time as a Unix timestamp.
	CreatedAt int64

	// HeightHint is the chain height at batch creation time.
	HeightHint uint32

	// BatchPSBT is the genesis transaction PSBT, if available.
	BatchPSBT []byte
}

// VerboseMintingBatch is the daemon's verbose view of a minting batch.
type VerboseMintingBatch struct {
	// Batch is the batch itself.
	Batch MintingBatch

	// UnsealedAssets contains pending group witness details when requested.
	UnsealedAssets []UnsealedMintAsset
}

// FungibleAssetSpec describes a fungible token created through the high-level
// issuer surface. New fungible assets are grouped so their AssetRef remains
// stable across future issuances.
type FungibleAssetSpec struct {
	// Name is the human-readable asset name.
	Name string

	// Amount is the number of units in the first issuance.
	Amount uint64

	// AssetMeta is the optional metadata committed to the first issuance.
	AssetMeta *AssetMeta

	// AssetVersion is the asset encoding version.
	AssetVersion AssetVersion

	// DecimalDisplay is the wallet display precision for the asset.
	DecimalDisplay uint32

	// ScriptKey is the optional custom script key for the first issuance.
	ScriptKey *ScriptKey

	// EnableSupplyCommitments enables supply commitments for the asset.
	EnableSupplyCommitments bool
}

// NFTSpec describes a non-fungible token created through the high-level issuer
// surface. The same spec is used for standalone NFTs, first collection items,
// and additional collection items.
type NFTSpec struct {
	// Name is the human-readable item name.
	Name string

	// AssetMeta is the optional metadata committed to the NFT.
	AssetMeta *AssetMeta

	// AssetVersion is the asset encoding version.
	AssetVersion AssetVersion

	// ScriptKey is the optional custom script key for the NFT.
	ScriptKey *ScriptKey
}

// CollectionMintResult is returned when creating a new NFT collection.
type CollectionMintResult struct {
	// Collection is the collection-level entity keyed by group AssetRef.
	Collection *Collection

	// FirstItem is the NFT item minted to create the collection.
	FirstItem *Asset
}

// MintAsset describes a brand-new protocol asset to add to a minting batch.
type MintAsset struct {
	// AssetVersion is the asset encoding version.
	AssetVersion AssetVersion

	// AssetType is the type of asset to mint.
	AssetType AssetType

	// Name is the asset tag.
	Name string

	// AssetMeta is the optional metadata committed to the genesis.
	AssetMeta *AssetMeta

	// InitialSupply is the number of units in the first issuance.
	InitialSupply uint64

	// AllowIssuance creates the asset with a group key so future issuances are
	// possible.
	AllowIssuance bool

	// DecimalDisplay is the wallet display precision for the asset.
	DecimalDisplay uint32

	// ScriptKey is the optional custom script key for the first issuance.
	ScriptKey *ScriptKey

	// GroupInternalKey is the internal key for a newly issuable asset.
	GroupInternalKey *KeyDescriptor

	// GroupTapscriptRoot is the optional tapscript root for a newly issuable
	// asset.
	GroupTapscriptRoot []byte

	// ExternalGroupKey enables external signing for future issuance.
	ExternalGroupKey *ExternalKey

	// EnableSupplyCommitments enables supply commitments for a newly issuable
	// asset.
	EnableSupplyCommitments bool
}

// MintAssetRequest adds a brand-new protocol asset to the pending mint batch.
type MintAssetRequest struct {
	// Asset is the asset to mint.
	Asset *MintAsset

	// ShortResponse asks the daemon to omit existing batch assets.
	ShortResponse bool
}

// MintIssuance describes an additional issuance/tranche for an existing
// grouped asset.
type MintIssuance struct {
	// AssetRef identifies the asset to issue more units of. It must resolve to a
	// group key.
	AssetRef AssetRef

	// Name is the asset tag of the existing asset. tapd still requires it on the
	// underlying mint request.
	Name string

	// AssetType is the type of the existing asset.
	AssetType AssetType

	// AssetMeta is the optional metadata committed to this issuance.
	AssetMeta *AssetMeta

	// Amount is the number of units to issue in this tranche.
	Amount uint64

	// AssetVersion is the asset encoding version.
	AssetVersion AssetVersion

	// DecimalDisplay is the display precision for the issuance metadata.
	DecimalDisplay uint32

	// ScriptKey is the optional custom script key for the issuance.
	ScriptKey *ScriptKey

	// ExternalGroupKey enables external signing for the group issuance.
	ExternalGroupKey *ExternalKey
}

// MintIssuanceRequest adds an additional issuance to the pending mint batch.
type MintIssuanceRequest struct {
	// Issuance is the issuance to mint.
	Issuance *MintIssuance

	// ShortResponse asks the daemon to omit existing batch assets.
	ShortResponse bool
}

// FundBatchRequest funds the current pending mint batch.
type FundBatchRequest struct {
	// ShortResponse asks the daemon to omit batch asset details.
	ShortResponse bool

	// FeeRate is the optional fee rate. Zero uses the daemon default.
	FeeRate FeeRate

	// BatchSibling is the optional tapscript sibling for the batch output.
	BatchSibling *BatchSibling
}

// SealBatchRequest seals the current funded mint batch.
type SealBatchRequest struct {
	// ShortResponse asks the daemon to omit batch asset details.
	ShortResponse bool

	// GroupWitnesses authorizes assets into externally signed groups.
	GroupWitnesses []GroupWitness

	// SignedGroupVirtualPSBTs are externally signed group virtual PSBTs.
	SignedGroupVirtualPSBTs []string
}

// FinalizeBatchRequest finalizes the current pending mint batch.
type FinalizeBatchRequest struct {
	// ShortResponse asks the daemon to omit batch asset details.
	ShortResponse bool

	// FeeRate is the optional fee rate. Zero uses the daemon default.
	FeeRate FeeRate

	// BatchSibling is the optional tapscript sibling for the batch output.
	BatchSibling *BatchSibling
}

// CancelBatchResponse reports the cancelled batch key.
type CancelBatchResponse struct {
	// BatchKey is the internal public key of the cancelled batch.
	BatchKey PubKey
}

// ListBatchesRequest queries known mint batches.
type ListBatchesRequest struct {
	// BatchKey filters the response to a single batch when set.
	BatchKey *PubKey

	// Verbose asks the daemon to include unsealed asset details.
	Verbose bool
}
