package tapsdk

import (
	"testing"

	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// TestCustomAnchorActivationEvidence preserves the released tapd rules and
// their error precedence without requiring an exported upstream helper.
func TestCustomAnchorActivationEvidence(t *testing.T) {
	t.Parallel()

	_, base, key := newAssetProofPathBase(t)
	require.NoError(t, checkActivationEvidence(base))
	split := newAssetProofPathSplitTransition(t, base, key)
	require.NoError(t, checkActivationEvidence(split))
	pkg := newCustomAnchorTestFixture(t).sealed
	regular, err := proof.Decode(pkg.ProofUpdates[0].ProofBlob)
	require.NoError(t, err)
	require.NoError(t, checkActivationEvidence(regular))
	tests := []struct {
		name   string
		base   *proof.Proof
		mutate func(*proof.Proof)
		want   error
	}{
		{
			name: "old transition", base: regular,
			mutate: func(p *proof.Proof) {
				p.Version = proof.TransitionV0
				p.InclusionProof.CommitmentProof = nil
			},
			want: proof.ErrTransitionV1Required,
		},
		{
			name: "regular spender evidence", base: regular,
			mutate: func(p *proof.Proof) {
				p.InclusionProof.CommitmentProof = nil
			},
			want: proof.ErrMissingSpenderProofs,
		},
		{
			name: "split locator", base: split,
			mutate: func(p *proof.Proof) {
				p.RootLocatorProof = nil
				p.SplitRootProof = nil
			},
			want: proof.ErrMissingRootLocatorProof,
		},
		{
			name: "split root evidence", base: split,
			mutate: func(p *proof.Proof) {
				p.SplitRootProof = nil
			},
			want: proof.ErrMissingSplitSTXOProofs,
		},
		{
			name: "split spender evidence", base: split,
			mutate: func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.SpenderProofs = nil
			},
			want: proof.ErrMissingSpenderProofs,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := test.base.Bytes()
			require.NoError(t, err)
			p, err := proof.Decode(raw)
			require.NoError(t, err)
			test.mutate(p)
			require.ErrorIs(t, checkActivationEvidence(p), test.want)
		})
	}
}
