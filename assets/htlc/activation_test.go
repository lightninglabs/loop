package htlc

import (
	"context"
	"testing"

	assettest "github.com/lightninglabs/loop/assets/internal/test"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// activatedVerifier runs tapd's real proof and asset VM checks at mainnet's
// activation height. Only chain lookups are mocked, so setting a pre-activation
// proof height represents an authenticated historical confirmation.
type activatedVerifier struct {
	calls int
}

// GetInfo reports the supported backend that applies the activation rules.
func (*activatedVerifier) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	return &taprpc.GetInfoResponse{Version: testSupportedTapd}, nil
}

// VerifyProof delegates the complete history to tapd with activation enabled.
func (v *activatedVerifier) VerifyProof(ctx context.Context,
	req *taprpc.ProofFile, _ ...grpc.CallOption) (
	*taprpc.VerifyProofResponse, error) {

	v.calls++
	file, err := proof.DecodeFile(req.RawProofFile)
	if err != nil {
		return nil, err
	}
	verifierCtx := proof.MockVerifierCtx
	verifierCtx.ActivationHeight = fn.Some(proof.MainNetActivationHeight)
	_, err = file.Verify(ctx, verifierCtx)

	return &taprpc.VerifyProofResponse{Valid: err == nil}, err
}

// TestVerifyProofFileActivationBoundary checks full proof histories against
// the same activation setting in the kit and in tapd. Historical legacy
// transfers remain accepted, while new transfers need V1 root and spender
// evidence before the kit delegates cryptographic verification to tapd.
func TestVerifyProofFileActivationBoundary(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	kit := mainnetKit(t, f.kit)
	internalKey, err := kit.GetAggregateKey()
	require.NoError(t, err)
	sibling, err := kit.GetSiblingPreimage()
	require.NoError(t, err)
	issuance := htlcIssuanceProof(t, f)
	issuance.BlockHeight = proof.MainNetActivationHeight - 2
	legacy := htlcTransferProof(t, issuance, internalKey, &sibling)
	legacy.BlockHeight = proof.MainNetActivationHeight - 1
	verifier := &activatedVerifier{}
	outpoint := legacy.OutPoint()
	accepted, err := kit.VerifyProofFile(
		t.Context(), verifier, encodeHtlcProofFile(t, *issuance, *legacy),
		&outpoint,
	)
	require.NoError(t, err)
	require.Equal(t, outpoint, accepted.OutPoint())
	require.Equal(t, 1, verifier.calls)

	// A new transition confirmed exactly at activation loses the historical
	// exemption even if it was constructed or broadcast before the boundary.
	legacy.BlockHeight = proof.MainNetActivationHeight
	_, err = kit.VerifyProofFile(
		t.Context(), verifier, encodeHtlcProofFile(t, *issuance, *legacy),
		&outpoint,
	)
	require.ErrorIs(t, err, proof.ErrTransitionV1Required)
	require.Equal(t, 1, verifier.calls)
	_, err = verifier.VerifyProof(
		t.Context(), encodeHtlcProofFile(t, *issuance, *legacy),
	)
	require.ErrorIs(t, err, proof.ErrTransitionV1Required)

	// Relabeling the legacy proof cannot repair missing on-chain spender
	// commitments. Both acceptance layers must continue to reject it.
	legacy.Version = proof.TransitionV1
	_, err = kit.VerifyProofFile(
		t.Context(), verifier, encodeHtlcProofFile(t, *issuance, *legacy),
		&outpoint,
	)
	require.ErrorIs(t, err, proof.ErrMissingSpenderProofs)
	require.Equal(t, 2, verifier.calls)
	_, err = verifier.VerifyProof(
		t.Context(), encodeHtlcProofFile(t, *issuance, *legacy),
	)
	require.ErrorIs(t, err, proof.ErrMissingSpenderProofs)

	// A genuine new split funding proof carries the root locator as well as
	// STXO and spender proofs. Dropping each required piece is rejected by
	// the public acceptance path, even with an otherwise complete history.
	source := f.proof.Asset.Copy()
	source.Amount = 1500
	genesis := sourceProof(t, source)
	genesis.PrevOut = source.Genesis.FirstPrevOut
	genesis.GenesisReveal = &source.Genesis
	genesis.BlockHeight = proof.MainNetActivationHeight - 1
	funding := fundHtlcProof(t, f, fundingLayout{source: source.Amount})
	funding.BlockHeight = proof.MainNetActivationHeight
	funding.Version = proof.TransitionV1
	outpoint = funding.OutPoint()
	accepted, err = kit.VerifyProofFile(
		t.Context(), verifier, encodeHtlcProofFile(t, *genesis, *funding),
		&outpoint,
	)
	require.NoError(t, err)
	require.Equal(t, outpoint, accepted.OutPoint())
	for _, test := range []struct {
		name   string
		mutate func(*proof.Proof)
		err    error
	}{
		{
			name: "root locator",
			mutate: func(p *proof.Proof) {
				p.RootLocatorProof = nil
			},
			err: proof.ErrMissingRootLocatorProof,
		},
		{
			name: "root STXO proofs",
			mutate: func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.STXOProofs = nil
			},
			err: proof.ErrMissingSplitSTXOProofs,
		},
		{
			name: "root spender proofs",
			mutate: func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.SpenderProofs = nil
			},
			err: proof.ErrMissingSpenderProofs,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := proof.DecodeFile(encodeHtlcProofFile(
				t, *genesis, *funding,
			).RawProofFile)
			require.NoError(t, err)
			stripped, err := file.LastProof()
			require.NoError(t, err)
			test.mutate(stripped)
			history := encodeHtlcProofFile(t, *genesis, *stripped)
			calls := verifier.calls
			_, err = kit.VerifyProofFile(
				t.Context(), verifier, history, &outpoint,
			)
			require.ErrorIs(t, err, test.err)
			require.Equal(t, calls, verifier.calls)
			_, err = verifier.VerifyProof(t.Context(), history)
			require.ErrorIs(t, err, test.err)
		})
	}
}

// TestLegacyFundingRecoveryAfterActivation spends an accepted legacy transfer
// through both HTLC paths after activation. Signing checks the entire new
// transfer, and tapd then verifies every generated V1 output proof against the
// original historical provenance, including real STXO/spender commitments.
func TestLegacyFundingRecoveryAfterActivation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		path := preimagePath
		if timeout {
			path = timeoutPath
		}
		t.Run(path, func(t *testing.T) {
			f := newWitnessFixture(t, SuccessSequence)
			f.kit = mainnetKit(t, f.kit)
			internalKey, err := f.kit.GetAggregateKey()
			require.NoError(t, err)
			sibling, err := f.kit.GetSiblingPreimage()
			require.NoError(t, err)
			issuance := htlcIssuanceProof(t, f)
			issuance.BlockHeight = proof.MainNetActivationHeight - 2
			legacy := htlcTransferProof(t, issuance, internalKey, &sibling)
			legacy.BlockHeight = proof.MainNetActivationHeight - 1
			outpoint := legacy.OutPoint()
			f.proof, err = f.kit.VerifyProofFile(
				t.Context(), &activatedVerifier{},
				encodeHtlcProofFile(t, *issuance, *legacy), &outpoint,
			)
			require.NoError(t, err)
			f.packet.UnsignedTx.TxIn[f.assetInIndex].PreviousOutPoint = outpoint
			f.prevOutputs[f.assetInIndex] = legacy.AnchorTx.TxOut[0]
			f.packet.Inputs[f.assetInIndex].WitnessUtxo =
				f.prevOutputs[f.assetInIndex]
			f.transfer = assettest.Sweep(
				t, []*proof.Proof{f.proof}, f.packet, opTrueWitness(t),
			)
			signer := &localSigner{
				privateKey: f.receiverKey, expectedInput: f.assetInIndex,
			}
			var witness *SpendWitness
			if timeout {
				f.packet.UnsignedTx.TxIn[f.assetInIndex].Sequence =
					f.kit.CsvExpiry()
				signer.privateKey = f.senderKey
				witness, err = f.kit.CreateTimeoutWitness(
					t.Context(), signer, f.proof, f.packet,
					keychain.KeyLocator{}, f.transfer,
				)
			} else {
				witness, err = f.kit.CreatePreimageWitness(
					t.Context(), signer, f.proof, f.packet,
					keychain.KeyLocator{}, f.preimage, f.transfer,
				)
			}
			require.NoError(t, err)
			require.Equal(t, 1, signer.calls)
			f.executeWitness(t, witness)

			commitments, err := tapsend.CreateOutputCommitments(
				f.transfer.Packets, tapsend.WithNoSTXOProofs(),
			)
			require.NoError(t, err)
			packet := f.transfer.Packets[0]
			for idx := range packet.Outputs {
				suffix, err := tapsend.CreateProofSuffix(
					f.packet.UnsignedTx, f.packet.Outputs, packet,
					commitments, idx, f.transfer.Packets,
					proof.WithVersion(proof.TransitionV1),
				)
				require.NoError(t, err)
				suffix.BlockHeight = proof.MainNetActivationHeight + 144
				response, err := (&activatedVerifier{}).VerifyProof(
					t.Context(), encodeHtlcProofFile(
						t, *issuance, *legacy, *suffix,
					),
				)
				require.NoError(t, err)
				require.True(t, response.Valid)
			}
		})
	}
}
