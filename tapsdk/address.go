package tapsdk

// AddressVersion represents the version of a Taproot Asset address format.
// Values match taprpc.AddrVersion enum (0 is unspecified).
type AddressVersion uint8

const (
	// AddressVersionV0 is the initial address version using asset ID only.
	AddressVersionV0 AddressVersion = 1

	// AddressVersionV1 is the address version with improved encoding.
	AddressVersionV1 AddressVersion = 2

	// AddressVersionV2 supports group keys and optional amounts.
	AddressVersionV2 AddressVersion = 3
)

// AssetVersion represents the version of asset encoding.
type AssetVersion uint8

const (
	// AssetVersionV0 is the initial asset version.
	AssetVersionV0 AssetVersion = 0

	// AssetVersionV1 is the asset version with witness stripping support.
	AssetVersionV1 AssetVersion = 1
)

// Address represents a Taproot Asset address for receiving assets.
type Address struct {
	// Encoded is the bech32m encoded Taproot Asset address string.
	// This is the canonical representation used for sharing with senders.
	Encoded string

	// AssetRef is the SDK's user-facing asset identifier for the address.
	AssetRef AssetRef

	// AssetType indicates whether this is a normal or collectible asset.
	AssetType AssetType

	// Amount is the number of asset units expected at this address.
	// For V2 addresses, this may be zero to allow sender-chosen amounts.
	Amount uint64

	// ScriptKey is the Taproot output key the asset will be locked to.
	ScriptKey PubKey

	// InternalKey is the internal key for the on-chain anchor output.
	InternalKey PubKey

	// TapscriptSibling is the optional serialized tapscript sibling preimage.
	// Used when additional script paths exist alongside the asset commitment.
	TapscriptSibling []byte

	// TaprootOutputKey is the tweaked key for the on-chain Bitcoin output.
	TaprootOutputKey XOnlyPubKey

	// ProofCourierAddr is the address of the proof courier service.
	// For V2 addresses, this is mandatory and must be an auth mailbox URL.
	ProofCourierAddr string

	// AssetVersion is the asset version for transfers to this address.
	AssetVersion AssetVersion

	// AddressVersion is the address format version (V0, V1, or V2).
	AddressVersion AddressVersion
}

// NewAddressRequest contains parameters for generating a new Taproot Asset
// address.
type NewAddressRequest struct {
	// AssetRef is the SDK's user-facing identifier for the asset to receive.
	// For V0/V1 addresses this must resolve to a concrete issuance asset ID.
	// For V2 addresses it may resolve to either an issuance asset ID or a
	// group key.
	AssetRef AssetRef

	// Amount is the number of asset units to receive.
	// Required for V0/V1 addresses. Optional for V2 addresses (zero means
	// sender chooses the amount).
	Amount uint64

	// ScriptKey is an optional custom script key for the receiving asset.
	// If nil, tapd derives a BIP-86 key from its wallet.
	// NOTE: If set, InternalKey must also be set.
	ScriptKey *ScriptKey

	// InternalKey is an optional custom internal key for the anchor output.
	// If nil, tapd derives a key from its wallet.
	// NOTE: If set, ScriptKey must also be set.
	InternalKey *KeyDescriptor

	// TapscriptSibling is an optional tapscript sibling preimage for
	// additional script paths in the Taproot tree.
	TapscriptSibling []byte

	// ProofCourierAddr is the optional proof courier address.
	// If empty, tapd uses its configured default.
	// For V2 addresses, must be a valid auth mailbox URL.
	ProofCourierAddr string

	// AssetVersion is the asset version for transfers to this address.
	// Defaults to the latest version if not specified.
	AssetVersion *AssetVersion

	// AddressVersion is the address format version to use.
	// Defaults to the latest version if not specified.
	AddressVersion *AddressVersion

	// SkipProofCourierConnCheck skips the connectivity check to the proof
	// courier when creating the address. Useful for offline address
	// creation.
	SkipProofCourierConnCheck bool
}

// AddressQuery contains filters for querying stored addresses.
type AddressQuery struct {
	// CreatedAfter filters addresses created after this Unix timestamp.
	// Zero means no lower bound.
	CreatedAfter int64

	// CreatedBefore filters addresses created before this Unix timestamp.
	// Zero means no upper bound.
	CreatedBefore int64

	// Limit is the maximum number of addresses to return.
	// Zero means use server default.
	Limit int32

	// Offset is the number of addresses to skip for pagination.
	Offset int32
}

// AddressEventStatus represents the status of an incoming address transfer.
type AddressEventStatus uint8

const (
	// AddressEventStatusUnknown indicates an unknown status.
	AddressEventStatusUnknown AddressEventStatus = 0

	// AddressEventStatusTransactionDetected means the on-chain transaction
	// was detected in the mempool or a block.
	AddressEventStatusTransactionDetected AddressEventStatus = 1

	// AddressEventStatusTransactionConfirmed means the transaction was
	// confirmed in a block.
	AddressEventStatusTransactionConfirmed AddressEventStatus = 2

	// AddressEventStatusProofReceived means the asset proof was received
	// from the sender via the proof courier.
	AddressEventStatusProofReceived AddressEventStatus = 3

	// AddressEventStatusCompleted means the transfer is complete and the
	// asset is available in the wallet.
	AddressEventStatusCompleted AddressEventStatus = 4
)

// AddressEvent represents an incoming asset transfer event for an address.
type AddressEvent struct {
	// CreationTime is the Unix timestamp when the event was created.
	CreationTime uint64

	// Address is the address that received the transfer.
	Address *Address

	// Status is the current status of the incoming transfer.
	Status AddressEventStatus

	// Outpoint is the Bitcoin outpoint containing the inbound transfer.
	// Format: "txid:index"
	Outpoint string

	// UTXOAmountSat is the amount in satoshis transferred on-chain.
	// This is independent of the asset amount.
	UTXOAmountSat uint64

	// TaprootSibling is the taproot sibling hash used for the output.
	TaprootSibling []byte

	// ConfirmationHeight is the block height where the output was confirmed.
	// Zero means the output is unconfirmed.
	ConfirmationHeight uint32

	// HasProof indicates whether a proof file exists for this transfer.
	HasProof bool
}

// SortDirection specifies the sort order for queries.
// Values match taprpc.SortDirection enum.
type SortDirection uint8

const (
	// SortDescending sorts results in descending order.
	SortDescending SortDirection = 0

	// SortAscending sorts results in ascending order.
	SortAscending SortDirection = 1
)

// AddressReceivesQuery contains filters for querying address receive events.
type AddressReceivesQuery struct {
	// FilterAddr filters events for a specific address string.
	// Empty means return events for all addresses.
	FilterAddr string

	// FilterStatus filters events by status.
	// Zero (Unknown) means return all statuses.
	FilterStatus AddressEventStatus

	// StartTimestamp filters events created at or after this Unix timestamp.
	// Zero means no lower bound.
	StartTimestamp uint64

	// EndTimestamp filters events created at or before this Unix timestamp.
	// Zero means no upper bound.
	EndTimestamp uint64

	// Offset is the number of events to skip for pagination.
	Offset int32

	// Limit is the maximum number of events to return.
	// Zero means use server default.
	Limit int32

	// Direction specifies sort order by creation time.
	Direction SortDirection
}
