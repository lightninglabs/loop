package tapsdk

import "github.com/lightninglabs/taproot-assets/proof"

// checkActivationEvidence retains tapd v0.8.6's missingActivationEvidence
// requirements when using a compatible tapd revision without the exported
// Proof.CheckActivationEvidence method. This checks required fields only;
// the native proof verifier still checks their cryptographic validity. Remove
// this adapter once the dependency exports the method.
func checkActivationEvidence(p *proof.Proof) error {
	if p.Asset.IsGenesisAsset() {
		return nil
	}
	if p.Version == proof.TransitionV0 {
		return proof.ErrTransitionV1Required
	}
	isSplit := p.Asset.HasSplitCommitmentWitness()
	if (isSplit || p.Asset.SplitCommitmentRoot != nil) &&
		p.RootLocatorProof == nil {

		return proof.ErrMissingRootLocatorProof
	}
	root := &p.InclusionProof
	if isSplit {
		root = p.SplitRootProof
		if root == nil || root.CommitmentProof == nil ||
			len(root.CommitmentProof.STXOProofs) == 0 {

			return proof.ErrMissingSplitSTXOProofs
		}
	}
	if root.CommitmentProof == nil ||
		len(root.CommitmentProof.SpenderProofs) == 0 {

		return proof.ErrMissingSpenderProofs
	}
	return nil
}
