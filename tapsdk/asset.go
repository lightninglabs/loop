package tapsdk

import (
	"encoding/hex"
	"fmt"
)

// AssetID is the 32-byte Taproot Asset identifier.
type AssetID [32]byte

// String returns the hex encoding of the asset ID.
func (id AssetID) String() string {
	return hex.EncodeToString(id[:])
}

// IsZero returns true when id is the all-zero sentinel value.
func (id AssetID) IsZero() bool {
	return id == AssetID{}
}

// PrevID represents a previous asset input to be spent. It is the Taproot
// Assets protocol-level identifier for an input asset.
type PrevID struct {
	// Outpoint is the bitcoin anchor output on chain.
	Outpoint Outpoint

	// IssuanceID is the 32-byte protocol-level asset identifier of the
	// previous asset tree.
	IssuanceID AssetID

	// ScriptKey is the tweaked Taproot output key.
	ScriptKey PubKey
}

// AssetPacket represents a fully constructed and signed asset transfer packet
// ready for broadcasting.
type AssetPacket struct {
	// AnchorTransaction is the raw bytes of the final Bitcoin anchor transaction.
	AnchorTransaction []byte

	// VirtualTransactions are the signed virtual asset transactions.
	VirtualTransactions [][]byte

	// PassiveAssetTransactions are the signed passive asset transactions.
	PassiveAssetTransactions [][]byte
}

// AssetType represents the type of asset.
type AssetType uint8

const (
	// AssetTypeNormal is tapd's name for a fungible asset.
	AssetTypeNormal AssetType = 0

	// AssetTypeCollectible is tapd's name for a non-fungible asset.
	AssetTypeCollectible AssetType = 1

	// AssetTypeFungible is the SDK business name for a fungible token.
	AssetTypeFungible = AssetTypeNormal

	// AssetTypeNFT is the SDK business name for a non-fungible token.
	AssetTypeNFT = AssetTypeCollectible
)

// IssuanceGenesis is the protocol-level genesis information for one concrete
// asset issuance/tranche.
type IssuanceGenesis struct {
	// FirstPrevOut is the outpoint of the first input of the genesis transaction.
	FirstPrevOut Outpoint

	// Tag is the asset tag.
	Tag string

	// MetaHash is the meta data hash.
	MetaHash Hash

	// IssuanceID is the unique 32-byte protocol-level identifier for this
	// specific issuance/tranche.
	IssuanceID AssetID

	// OutputIndex is the output index of the genesis transaction.
	OutputIndex uint32

	// Type is the asset type.
	Type AssetType
}

// AltLeaf represents an auxiliary leaf in the Taproot tree.
type AltLeaf struct {
	// Value is the raw bytes of the leaf.
	Value []byte
}

// AssetRecord is the low-level tapd wallet/output row for a concrete asset
// commitment. Most application code should use Asset, Collection, and Issuance
// returned by the high-level Wallet methods instead.
type AssetRecord struct {
	// AssetRef is the SDK's user-facing identifier inferred for this concrete
	// row. Grouped records use the group AssetRef; ungrouped records use the
	// issuance asset-ID AssetRef.
	AssetRef AssetRef

	// Version is the asset version.
	Version uint8

	// Genesis is the protocol issuance genesis for this concrete record.
	Genesis IssuanceGenesis

	// Amount is the asset amount.
	Amount uint64

	// LockTime is the CLTV lock time.
	LockTime uint64

	// RelativeLockTime is the CSV lock time.
	RelativeLockTime uint64

	// ScriptVersion is the script version.
	ScriptVersion uint16

	// ScriptKey is the script key.
	ScriptKey ScriptKey

	// AnchorOutpoint identifies this concrete wallet output when tapd
	// supplies its chain anchor. It is nil for records without an anchor.
	// Use it when exporting proofs for a reused script key.
	AnchorOutpoint *Outpoint

	// AltLeaves are the auxiliary leaves.
	AltLeaves []AltLeaf
}

// Asset is the SDK's user-facing business entity for a wallet asset.
//
// A grouped fungible token is one Asset keyed by its group AssetRef, with any
// underlying issuance/tranche records hidden by default. An ungrouped fungible
// has no group-level identifier, so it is surfaced as its own Asset keyed by
// asset-ID AssetRef. A non-fungible token is one Asset keyed by its unique
// asset-ID AssetRef. Collections are not Assets; use Collection and
// ListCollections for collection-level views.
type Asset struct {
	// AssetRef is the stable SDK identifier for this asset.
	AssetRef AssetRef

	// Type is the asset type.
	Type AssetType

	// Name is the human-readable asset name.
	Name string

	// MetaHash is the asset metadata hash.
	MetaHash Hash

	// Amount is the sum of wallet rows returned by the ListAssets request. With
	// the default request this is the wallet-known confirmed, unspent, unleased
	// amount. For grouped fungibles, this may be larger than the amount a
	// single-tranche builder path can spend in one transfer.
	Amount uint64

	// CollectionRef identifies the collection this NFT belongs to. It is nil
	// for fungible assets and standalone NFTs.
	CollectionRef *AssetRef
}

// Collection is a group of NFT assets. A collection is not itself an Asset.
type Collection struct {
	// AssetRef is the stable SDK identifier for this collection.
	AssetRef AssetRef

	// ItemCount is the number of wallet-known NFT items in this collection.
	ItemCount uint64
}

// Issuance is one concrete fungible asset issuance/tranche.
type Issuance struct {
	// AssetRef is the stable SDK identifier of the fungible asset this
	// issuance belongs to.
	AssetRef AssetRef

	// IssuanceID is the protocol-level asset ID for this tranche.
	IssuanceID AssetID

	// Name is the human-readable asset tag.
	Name string

	// MetaHash is the issuance metadata hash.
	MetaHash Hash

	// Amount is the sum of wallet rows returned by the ListIssuances request
	// for this tranche. With the default request this is wallet-known,
	// confirmed, unspent, and unleased amount, not total issued supply.
	Amount uint64
}

// Ref returns the AssetRef for this concrete issuance/tranche.
func (i Issuance) Ref() AssetRef {
	return AssetRefFromAssetID(i.IssuanceID)
}

// ParseAssetID parses a 32-byte asset ID from raw bytes.
func ParseAssetID(b []byte) (AssetID, error) {
	var id AssetID
	if len(b) != len(id) {
		return id, fmt.Errorf("asset ID must be %d bytes, was %d", len(id),
			len(b))
	}

	copy(id[:], b)
	return id, nil
}

// ParseAssetIDHex parses a hex-encoded asset ID.
func ParseAssetIDHex(s string) (AssetID, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return AssetID{}, err
	}

	return ParseAssetID(b)
}
