package tapsdk

import (
	"bytes"
	"fmt"
)

// ValidateOwnershipChallenge checks the wire-level ownership challenge. A nil
// or empty challenge means the proof is not challenge-bound.
func ValidateOwnershipChallenge(challenge []byte) error {
	if len(challenge) == 0 {
		return nil
	}
	if len(challenge) != 32 {
		return fmt.Errorf(
			"ownership challenge must be 32 bytes, was %d",
			len(challenge),
		)
	}

	var zero [32]byte
	if bytes.Equal(challenge, zero[:]) {
		return fmt.Errorf("ownership challenge must not be all zero")
	}

	return nil
}

// ProveOwnershipRequest specifies low-level parameters for proving ownership
// of one concrete asset output. Most application code should use
// Wallet.ProveOwnership with an AssetRef instead.
type ProveOwnershipRequest struct {
	// AssetRef identifies the concrete issuance/tranche being proven. The
	// low-level wallet-kit RPC proves ownership over a single
	// (issuance, script key, outpoint) tuple, so AssetRef must resolve to an
	// asset ID.
	AssetRef AssetRef

	// ScriptKey is the script key used to spend the asset.
	ScriptKey PubKey

	// Outpoint is the UTXO outpoint being proven.
	Outpoint Outpoint

	// Challenge is an optional non-zero 32-byte challenge to bind the proof.
	Challenge []byte
}

// OwnershipProof is one ownership proof and its wallet-resolved metadata.
type OwnershipProof struct {
	// AssetRef is the SDK handle proven by this entry. Grouped fungibles use
	// the group AssetRef; NFTs use the concrete item AssetRef.
	AssetRef AssetRef

	// IssuanceID is the protocol-level asset ID proven by this entry.
	IssuanceID AssetID

	// ScriptKey is the script key used to prove ownership.
	ScriptKey PubKey

	// Outpoint is the asset anchor outpoint verified by tapd.
	Outpoint Outpoint

	// Amount is the number of units held at the proven output.
	Amount uint64

	// ProofWithWitness is the full ownership proof with witness data.
	ProofWithWitness []byte
}

// OwnershipProofSet is a Wallet-level ownership proof result for a
// user-facing AssetRef.
type OwnershipProofSet struct {
	// AssetRef is the user-facing asset or collection handle requested by the
	// caller.
	AssetRef AssetRef

	// Proofs contains the concrete output proofs that satisfy the request.
	Proofs []OwnershipProof
}

// VerifyOwnershipRequest specifies parameters for verifying an
// ownership proof.
type VerifyOwnershipRequest struct {
	// ProofWithWitness is the full ownership proof to verify.
	ProofWithWitness []byte

	// Challenge is the optional non-zero 32-byte challenge that the prover
	// was expected to include.
	Challenge []byte
}

// VerifyOwnershipResponse is the result of verifying an ownership
// proof.
type VerifyOwnershipResponse struct {
	// Valid indicates whether the ownership proof is valid.
	Valid bool

	// Outpoint is the asset anchor outpoint verified by tapd.
	Outpoint Outpoint

	// BlockHash is the hash of the block containing the anchor output.
	BlockHash Hash

	// BlockHeight is the block height of the anchor output.
	BlockHeight uint32
}

// DeclareScriptKeyRequest specifies a script key to declare to the
// wallet.
type DeclareScriptKeyRequest struct {
	// ScriptKey is the script key to declare.
	ScriptKey ScriptKey
}
