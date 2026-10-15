package deposit

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
)

// TestProofVersionPolicy enforces frozen history and transition versions before
// invoking tapd, including versions inside nested additional-input histories.
func TestProofVersionPolicy(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, container := range []bool{false, true} {
			f := newWitnessFixture(t)
			p := *f.proof
			if !container {
				p.Version = 2
				_, err := f.kit.AnchorRootFromProofCommitment(
					&p, p.Asset.Amount,
				)
				require.ErrorContains(t, err,
					"unsupported transition proof version")
			}
			file, err := proof.NewFile(proof.V0, p)
			require.NoError(t, err)
			if container {
				file.Version = 1
			}
			if nested {
				outer := *f.proof
				outer.AdditionalInputs = []proof.File{*file}
				file, err = proof.NewFile(proof.V0, outer)
				require.NoError(t, err)
			}
			var encoded bytes.Buffer
			require.NoError(t, file.Encode(&encoded))
			verifier := &proofVerifierMock{
				response: &taprpc.VerifyProofResponse{Valid: true},
			}
			outpoint := f.proof.OutPoint()
			_, err = f.kit.VerifyProofFile(
				t.Context(), verifier,
				&taprpc.ProofFile{RawProofFile: encoded.Bytes()},
				&outpoint, f.proof.Asset.Amount,
			)
			require.ErrorContains(t, err, "version")
			require.Zero(t, verifier.calls)
		}
	}
}
