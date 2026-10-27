// Package assetproof checks the input inventory required by Loop contracts.
// Generic lineage and transition verification belongs to the SDK and tapd.
package assetproof

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	policy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
)

// VerifyCompleteAnchor requires a proof to disclose its entire anchor
// commitment, including alt leaves. This prevents a negotiated contract from
// hiding co-anchored assets whose owner could bypass its Bitcoin policy.
// It does not verify history, confirmation or current unspent status.
func VerifyCompleteAnchor(assetProof *proof.Proof) (*commitment.TapCommitment,
	error) {

	if err := validateInventoryProof(assetProof); err != nil {
		return nil, err
	}
	provenCommitment, err := assetProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof: %w", err)
	}
	if err := policy.ValidateCommitmentVersion(provenCommitment.Version); err != nil {
		return nil, err
	}

	// Rebuild the disclosed on-chain leaf and alt leaves at version two
	// and then at the legacy versions.
	anchorAsset := assetProof.Asset.Copy()
	if anchorAsset.HasSplitCommitmentWitness() &&
		*anchorAsset.PrevWitnesses[0].PrevID == asset.ZeroPrevID {

		anchorAsset.PrevWitnesses[0].SplitCommitment = nil
	}
	version := policy.CommitmentVersion
	rebuiltCommitment, err := commitment.FromAssets(&version, anchorAsset)
	if err != nil {
		return nil, fmt.Errorf("unable to rebuild asset anchor: %w", err)
	}
	err = rebuiltCommitment.MergeAltLeaves(
		asset.CopyAltLeaves(assetProof.AltLeaves),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof alt leaves: %w", err)
	}

	provenRoot := provenCommitment.TapscriptRoot(nil)
	if rebuiltCommitment.TapscriptRoot(nil) == provenRoot {
		return provenCommitment, nil
	}
	legacyCommitment, err := rebuiltCommitment.Downgrade()
	if err != nil {
		return nil, fmt.Errorf("unable to rebuild legacy asset anchor: %w",
			err)
	}
	if legacyCommitment.TapscriptRoot(nil) != provenRoot {
		return nil, fmt.Errorf("asset anchor does not match the proven " +
			"asset and alt leaves")
	}

	return provenCommitment, nil
}

// VerifyCompleteAnchors checks the union of disclosed active and passive assets
// at each input anchor. A native SDK request can preserve known co-anchored
// assets by supplying all their proofs; an undisclosed asset is rejected.
// This does not establish lineage, chain inclusion or current spendability.
func VerifyCompleteAnchors(proofs []*proof.Proof) error {
	if len(proofs) == 0 {
		return fmt.Errorf("asset input inventory is required")
	}
	groups := make(map[wire.OutPoint][]*proof.Proof)
	for _, p := range proofs {
		if err := validateInventoryProof(p); err != nil {
			return err
		}
		groups[p.OutPoint()] = append(groups[p.OutPoint()], p)
	}
	for point, group := range groups {
		if len(group) == 1 {
			if _, err := VerifyCompleteAnchor(group[0]); err != nil {
				return fmt.Errorf("input anchor %v: %w", point, err)
			}
			continue
		}
		if err := verifyDisclosedAnchor(group); err != nil {
			return fmt.Errorf("input anchor %v: %w", point, err)
		}
	}
	return nil
}

func validateInventoryProof(p *proof.Proof) error {
	if p == nil || p.InclusionProof.CommitmentProof == nil ||
		p.InclusionProof.InternalKey == nil || p.Asset.ScriptKey.PubKey == nil {

		return fmt.Errorf("complete asset inclusion proof is required")
	}
	if err := policy.ValidateTransitionVersion(p.Version); err != nil {
		return err
	}
	if !policy.SupportsAssetVersion(p.Asset.Version) {
		return fmt.Errorf("unsupported input asset version %d", p.Asset.Version)
	}
	for _, witness := range p.Asset.PrevWitnesses {
		if witness.SplitCommitment != nil &&
			(witness.PrevID == nil ||
				witness.SplitCommitment.RootAsset.ScriptKey.PubKey == nil) {

			return fmt.Errorf("split asset witness is incomplete")
		}
	}
	return nil
}

func verifyDisclosedAnchor(proofs []*proof.Proof) error {
	var disclosed []*asset.Asset
	altLeaves := make(map[[32]byte]asset.AltLeaf[asset.Asset])
	var expectedRoot chainhash.Hash
	for idx, p := range proofs {
		proven, err := p.VerifyProofs()
		if err != nil {
			return err
		}
		if err := policy.ValidateCommitmentVersion(proven.Version); err != nil {
			return err
		}
		root := proven.TapscriptRoot(nil)
		if idx == 0 {
			expectedRoot = root
		} else if root != expectedRoot {
			return fmt.Errorf("disclosed assets disagree on their anchor root")
		}
		a := p.Asset.Copy()
		if a.HasSplitCommitmentWitness() &&
			*a.PrevWitnesses[0].PrevID == asset.ZeroPrevID {

			a.PrevWitnesses[0].SplitCommitment = nil
		}
		disclosed = append(disclosed, a)
		for _, alt := range p.AltLeaves {
			key := alt.AssetCommitmentKey()
			if previous, ok := altLeaves[key]; ok {
				var old, next bytes.Buffer
				if err := previous.EncodeAltLeaf(&old); err != nil {
					return err
				}
				if err := alt.EncodeAltLeaf(&next); err != nil {
					return err
				}
				if !bytes.Equal(old.Bytes(), next.Bytes()) {
					return fmt.Errorf("conflicting disclosed alt leaves")
				}
			} else {
				altLeaves[key] = alt.Copy()
			}
		}
	}
	version := policy.CommitmentVersion
	rebuilt, err := commitment.FromAssets(&version, disclosed...)
	if err != nil {
		return err
	}
	leaves := make([]asset.AltLeaf[asset.Asset], 0, len(altLeaves))
	for _, leaf := range altLeaves {
		leaves = append(leaves, leaf)
	}
	if err := rebuilt.MergeAltLeaves(leaves); err != nil {
		return err
	}
	if rebuilt.TapscriptRoot(nil) == expectedRoot {
		return nil
	}
	legacy, err := rebuilt.Downgrade()
	if err != nil {
		return err
	}
	if legacy.TapscriptRoot(nil) != expectedRoot {
		return fmt.Errorf("input anchor contains undisclosed asset leaves")
	}
	return nil
}
