package tapsdk

// ProofFile represents an exported proof file for an asset.
type ProofFile struct {
	// RawProofFile is the serialized proof file containing the full
	// provenance chain for the asset.
	RawProofFile []byte

	// GenesisPoint is the outpoint of the asset's genesis transaction.
	GenesisPoint Outpoint
}

// ProofBundle is a wallet-level proof export for a user-facing AssetRef.
//
// A bundle can contain one or more proof files. For a single NFT/collectible
// or ungrouped asset this is normally one entry. For a grouped fungible asset,
// entries enumerate the wallet-known issuances/tranches behind the group ref.
type ProofBundle struct {
	// AssetRef is the user-facing asset or collection handle requested by
	// the caller.
	AssetRef AssetRef

	// Entries are the proof files that make up the bundle.
	Entries []ProofEntry
}

// ProofEntry is one proof file inside a ProofBundle.
type ProofEntry struct {
	// AssetRef is the user-facing handle for this entry. For grouped
	// fungibles this is the bundle group ref. For NFTs this is the
	// concrete item asset-ID ref.
	AssetRef AssetRef

	// IssuanceID is the protocol-level asset ID proven by this entry.
	IssuanceID AssetID

	// ScriptKey is the asset script key used to locate/export the proof.
	ScriptKey PubKey

	// Outpoint is the optional anchor outpoint used to locate/export the
	// proof when known.
	Outpoint *Outpoint

	// Amount is the number of units proven by this entry.
	Amount uint64

	// ProofFile is the serialized proof file for this entry.
	ProofFile []byte
}

// DecodedProof contains information extracted from a decoded proof.
type DecodedProof struct {
	// ProofAtDepth is the index depth of this proof (0 = latest).
	ProofAtDepth uint32

	// NumberOfProofs is the total number of proofs in the chain.
	NumberOfProofs uint32

	// AssetRef is the SDK's user-facing identifier for the asset.
	AssetRef AssetRef

	// IssuanceID is the 32-byte protocol-level identifier for the specific
	// issuance/tranche described by the proof.
	IssuanceID AssetID

	// ScriptKey is the 33-byte script key.
	ScriptKey PubKey

	// Amount is the number of asset units.
	Amount uint64

	// Outpoint is the output location in "txid:index" format.
	Outpoint Outpoint

	// AltLeaves are auxiliary Taproot leaves committed alongside the asset.
	// Each entry is the raw per-leaf TLV stream bytes (opaque).
	AltLeaves [][]byte

	// PrevIDs are the asset-level identifiers of the inputs referenced by this
	// proof's asset witnesses. These are required to derive STXO alt leaves for
	// v1 transfer proofs.
	PrevIDs []PrevID

	// IsIssuance is true if this is a genesis/issuance proof.
	IsIssuance bool
}

// VerifyProofResponse contains the result of a proof verification.
type VerifyProofResponse struct {
	// Valid indicates whether the proof file was valid.
	Valid bool

	// DecodedProof is the decoded last proof in the file if the proof
	// file was valid.
	DecodedProof *DecodedProof
}

// RegisteredAsset represents an asset that has been registered after
// importing a proof. This is returned when a receiver successfully imports
// a proof from an interactive transfer.
type RegisteredAsset struct {
	// AssetRef is the SDK's user-facing identifier for the asset.
	AssetRef AssetRef

	// IssuanceID is the 32-byte protocol-level identifier for the specific
	// issuance/tranche that was registered.
	IssuanceID AssetID

	// ScriptKey is the 33-byte script key for this asset.
	ScriptKey PubKey

	// Amount is the number of asset units.
	Amount uint64

	// Outpoint is the output location where this asset resides.
	Outpoint Outpoint
}
