package deposit

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// encodeProofFile serializes proof histories for the RPC boundary tests.
func encodeProofFile(t *testing.T, proofs ...proof.Proof) *taprpc.ProofFile {
	t.Helper()
	file, err := proof.NewFile(proof.V0, proofs...)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, file.Encode(&encoded))

	return &taprpc.ProofFile{RawProofFile: encoded.Bytes()}
}

// opTrueWitness returns a valid virtual script-path ownership/spend witness.
func opTrueWitness(t *testing.T) wire.TxWitness {
	t.Helper()
	_, leaf, _, control, err := htlc.CreateOpTrueLeaf()
	require.NoError(t, err)
	encoded, err := control.ToBytes()
	require.NoError(t, err)

	return wire.TxWitness{leaf.Script, encoded}
}

// reanchorDepositProof rebuilds a proof's commitment after changing its asset
// leaf, preserving the deposit's Bitcoin contract.
func reanchorDepositProof(t *testing.T, kit *Kit, p *proof.Proof) {
	t.Helper()
	version := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(&version, &p.Asset)
	require.NoError(t, err)
	_, inclusion, err := tapCommitment.Proof(
		p.Asset.TapCommitmentKey(), p.Asset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	sibling, err := kit.timeoutPathSibling()
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	script, err := tapscript.PayToAddrScript(
		*kit.muSig2Key.PreTweakedKey, siblingHash, *tapCommitment,
	)
	require.NoError(t, err)
	p.InclusionProof.CommitmentProof = &proof.CommitmentProof{
		Proof: *inclusion, TapSiblingPreimage: sibling,
	}
	p.AnchorTx = *p.AnchorTx.Copy()
	p.AnchorTx.TxOut[p.InclusionProof.OutputIndex].PkScript = script
}

// newTransferProof creates an OP_TRUE transfer from a fixture's genesis asset.
func newTransferProof(t *testing.T, fixture *witnessFixture) *proof.Proof {
	t.Helper()
	p := *fixture.proof
	p.Asset = *fixture.proof.Asset.Copy()
	p.Asset.PrevWitnesses = []asset.Witness{{
		PrevID: &asset.PrevID{
			OutPoint: fixture.proof.OutPoint(),
			ID:       p.Asset.ID(),
			ScriptKey: asset.ToSerialized(
				p.Asset.ScriptKey.PubKey,
			),
		},
		TxWitness: opTrueWitness(t),
	}}
	p.GenesisReveal = nil
	p.PrevOut = fixture.proof.OutPoint()
	p.AnchorTx = *p.AnchorTx.Copy()
	p.AnchorTx.TxIn[0].PreviousOutPoint = p.PrevOut
	reanchorDepositProof(t, fixture.kit, &p)

	return &p
}

// localProofVerifier runs tapd's proof-file verification path with only chain
// lookups mocked; commitment and asset VM verification remain real.
type localProofVerifier struct{}

// VerifyProof verifies a decoded file with the upstream proof verifier.
func (localProofVerifier) VerifyProof(ctx context.Context,
	req *taprpc.ProofFile, _ ...grpc.CallOption) (*taprpc.VerifyProofResponse,
	error) {

	file, err := proof.DecodeFile(req.RawProofFile)
	if err != nil {
		return nil, err
	}
	_, err = file.Verify(ctx, proof.MockVerifierCtx)

	return &taprpc.VerifyProofResponse{Valid: err == nil}, nil
}

// TestVerifyProofFileProvenance accepts real genesis and transfer proofs while
// rejecting standalone and nested ownership histories before invoking tapd.
func TestVerifyProofFileProvenance(t *testing.T) {
	fixture := newWitnessFixture(t)
	transfer := newTransferProof(t, fixture)
	for _, history := range [][]proof.Proof{
		{*fixture.proof},
		{*fixture.proof, *transfer},
	} {
		terminal := history[len(history)-1]
		outpoint := terminal.OutPoint()
		_, err := fixture.kit.VerifyProofFile(
			t.Context(), localProofVerifier{},
			encodeProofFile(t, history...), &outpoint,
			terminal.Asset.Amount,
		)
		require.NoError(t, err)
	}

	// Structural provenance must not replace cryptographic verification.
	invalidTransfer := *transfer
	invalidTransfer.Asset = *transfer.Asset.Copy()
	invalidTransfer.Asset.PrevWitnesses[0].TxWitness = nil
	outpoint := invalidTransfer.OutPoint()
	_, err := fixture.kit.VerifyProofFile(
		t.Context(), localProofVerifier{},
		encodeProofFile(t, *fixture.proof, invalidTransfer),
		&outpoint, invalidTransfer.Asset.Amount,
	)
	require.ErrorContains(t, err, "invalid deposit proof file")

	// This leaf has no issuance history and claims an inflated amount,
	// yet the real verifier accepts its public OP_TRUE ownership challenge.
	ownership := *transfer
	ownership.Asset = *transfer.Asset.Copy()
	ownership.Asset.Amount++
	ownership.ChallengeWitness = opTrueWitness(t)
	reanchorDepositProof(t, fixture.kit, &ownership)
	response, err := (localProofVerifier{}).VerifyProof(
		t.Context(), encodeProofFile(t, ownership),
	)
	require.NoError(t, err)
	require.True(t, response.Valid)

	// Wrap a file in AdditionalInputs, including more than one level to
	// ensure the restriction applies throughout the input tree.
	wrap := func(history []proof.Proof) []proof.Proof {
		inputFile, err := proof.NewFile(proof.V0, history...)
		require.NoError(t, err)
		parent := *fixture.proof
		parent.AdditionalInputs = []proof.File{*inputFile}
		return []proof.Proof{parent}
	}
	emptyChallenge := ownership
	emptyChallenge.ChallengeWitness = wire.TxWitness{}
	tests := []struct {
		name    string
		history []proof.Proof
		err     string
	}{
		{
			name: "empty file", err: "history is empty",
		},
		{
			name: "ownership", history: []proof.Proof{ownership},
			err: "ownership challenge",
		},
		{
			name:    "empty challenge witness",
			history: []proof.Proof{emptyChallenge},
			err:     "ownership challenge",
		},
		{
			name: "missing genesis", history: []proof.Proof{*transfer},
			err: "must start at genesis",
		},
		{
			name:    "later ownership challenge",
			history: []proof.Proof{*fixture.proof, ownership},
			err:     "ownership challenge",
		},
		{
			name:    "ownership additional input",
			history: wrap([]proof.Proof{ownership}),
			err:     "ownership challenge",
		},
		{
			name:    "deep ownership additional input",
			history: wrap(wrap([]proof.Proof{ownership})),
			err:     "ownership challenge",
		},
		{
			name:    "additional input missing genesis",
			history: wrap([]proof.Proof{*transfer}),
			err:     "must start at genesis",
		},
		{
			name:    "empty additional input",
			history: wrap(nil), err: "history is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier := &proofVerifierMock{
				response: &taprpc.VerifyProofResponse{Valid: true},
			}
			outpoint := fixture.proof.OutPoint()
			amount := fixture.proof.Asset.Amount
			if len(test.history) > 0 {
				terminal := test.history[len(test.history)-1]
				outpoint = terminal.OutPoint()
				amount = terminal.Asset.Amount
			}
			_, err := fixture.kit.VerifyProofFile(
				t.Context(), verifier,
				encodeProofFile(t, test.history...),
				&outpoint, amount,
			)
			require.ErrorContains(t, err, test.err)
			require.Zero(t, verifier.calls)
		})
	}
}

// TestValidateProofProvenanceNested verifies that complete input histories
// pass the structural gate and that traversal respects cancellation. Tapd
// remains responsible for validating the transitions connecting these files.
func TestValidateProofProvenanceNested(t *testing.T) {
	fixture := newWitnessFixture(t)
	file, err := proof.NewFile(proof.V0, *fixture.proof)
	require.NoError(t, err)
	for range 3 {
		parent := *fixture.proof
		parent.AdditionalInputs = []proof.File{*file}
		file, err = proof.NewFile(proof.V0, parent)
		require.NoError(t, err)
	}
	require.NoError(t, validateProofProvenance(t.Context(), file))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, validateProofProvenance(ctx, file), context.Canceled)
}
