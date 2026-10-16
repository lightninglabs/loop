// Package policy freezes the wire versions of the legacy deposit contract.
// New contracts use V1 assets and addresses with V2 commitments. Legacy input
// assets and transition proofs remain accepted under the explicit rules below;
// chain-dependent proof activation is still enforced by the full verifier.
package policy

import (
	"fmt"

	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
)

const (
	// AddressVersion is the predictable non-interactive address format.
	AddressVersion = address.V1

	// AssetVersion is used for newly created contract asset leaves.
	AssetVersion = asset.V1

	// PacketVersion selects the commitment format of newly funded contracts.
	PacketVersion = tappsbt.V1

	// CommitmentVersion is required by PacketVersion.
	CommitmentVersion = commitment.TapCommitmentV2

	// ProofFileVersion is the only supported proof history container.
	ProofFileVersion = proof.V0

	// TransitionVersion includes the activation-required STXO proofs.
	TransitionVersion = proof.TransitionV1

	// RPCAssetVersion explicitly selects AssetVersion in address requests.
	RPCAssetVersion = taprpc.AssetVersion_ASSET_VERSION_V1

	// RPCAddressVersion explicitly selects AddressVersion in address requests.
	RPCAddressVersion = taprpc.AddrVersion_ADDR_VERSION_V1
)

// ValidateTransitionVersion rejects unrecognized transition proof semantics
// even when only the commitment proofs, rather than the history, are checked.
func ValidateTransitionVersion(version proof.TransitionVersion) error {
	if version != proof.TransitionV0 && version != TransitionVersion {
		return fmt.Errorf("unsupported transition proof version %d", version)
	}
	return nil
}

// SupportsAssetVersion permits the two historical asset leaf encodings.
func SupportsAssetVersion(version asset.Version) bool {
	return version == asset.V0 || version == AssetVersion
}

// SupportsPacketVersion permits both historical virtual packet encodings.
func SupportsPacketVersion(version tappsbt.VPacketVersion) bool {
	return version == tappsbt.V0 || version == PacketVersion
}

// ValidateCommitmentVersion permits the three historical commitment formats.
func ValidateCommitmentVersion(version commitment.TapCommitmentVersion) error {
	switch version {
	case commitment.TapCommitmentV0, commitment.TapCommitmentV1,
		CommitmentVersion:

		return nil
	default:
		return fmt.Errorf("unsupported asset commitment version %d", version)
	}
}
