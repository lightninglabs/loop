package tapsdk

// BurnAssetRequest specifies parameters for burning asset units.
type BurnAssetRequest struct {
	// AssetRef identifies which asset to burn units from.
	AssetRef AssetRef

	// AmountToBurn is the number of asset units to burn. Must be
	// greater than zero.
	AmountToBurn uint64

	// ConfirmationText must be set to "assets will be destroyed" for
	// the burn to succeed. This is a safety check.
	ConfirmationText string

	// Note is optional user-defined metadata for this burn.
	Note string
}

// BurnAssetResponse contains the result of a burn operation.
type BurnAssetResponse struct {
	// BurnTransfer is the asset transfer containing the burn output.
	BurnTransfer *AssetTransfer

	// BurnProof is the transition proof for the burn output.
	BurnProof *DecodedProof
}

// Burn is the high-level result of burning asset units through Wallet.Burn.
type Burn struct {
	// AssetRef is the user-facing identifier requested by the caller.
	AssetRef AssetRef

	// Amount is the number of units burned.
	Amount uint64

	// Note is optional user-defined metadata for this burn.
	Note string

	// Transfer is the wallet transfer that committed the burn.
	Transfer *AssetTransfer

	// Proof is the transition proof for the burn output.
	Proof *DecodedProof
}

// ListBurnsRequest specifies filters for listing asset burns.
type ListBurnsRequest struct {
	// AssetRef filters by the burnt asset.
	AssetRef *AssetRef

	// AnchorTxid filters by the anchor transaction id.
	AnchorTxid *Hash
}

// BurnRecord represents a single asset burn event from wallet history.
type BurnRecord struct {
	// Note is user-defined metadata for the burn.
	Note string

	// AssetRef is the SDK's user-facing identifier for the burnt asset.
	AssetRef AssetRef

	// CollectionRef identifies the parent collection when the burnt asset
	// is an NFT collection item. It is nil for standalone NFTs and
	// fungible assets.
	CollectionRef *AssetRef

	// Type is the asset type reported by tapd for this burn. Use this
	// field for asset typing; AssetRef is the stable handle used to key
	// the burn record.
	Type AssetType

	// IssuanceID is the specific issuance/tranche that was burnt.
	IssuanceID AssetID

	// Amount is the number of burnt units.
	Amount uint64

	// AnchorTxid is the txid of the anchor transaction.
	AnchorTxid Hash
}
