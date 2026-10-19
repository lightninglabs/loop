package htlc

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/internal/policy"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// kitParams returns the parameters that construct the given kit.
func kitParams(kit *SwapKit) Params {
	addressParams := kit.AddressParams()

	return Params{
		SenderInternalPubKey:   kit.SenderInternalPubKey(),
		ReceiverInternalPubKey: kit.ReceiverInternalPubKey(),
		SenderScriptKey:        kit.SenderScriptKey(),
		ReceiverScriptKey:      kit.ReceiverScriptKey(),
		AssetID:                kit.AssetID(),
		Amount:                 kit.Amount(),
		SwapHash:               kit.SwapHash(),
		CsvExpiry:              kit.CsvExpiry(),
		AddressParams:          &addressParams,
	}
}

// mainnetKit returns the kit's contract on mainnet, where the Taproot Assets
// v0.8.5 activation rules apply to every proof claiming no earlier height.
func mainnetKit(t *testing.T, kit *SwapKit) *SwapKit {
	t.Helper()

	params := kitParams(kit)
	params.AddressParams = &address.MainNetTap
	mainnet, err := NewSwapKit(AssetHTLCV0, params)
	require.NoError(t, err)

	return mainnet
}

// anchorProofUnder anchors a copy of the asset alone in a version two
// commitment under the given internal key and optional tapscript sibling, and
// returns its structurally valid proof.
func anchorProofUnder(t *testing.T, htlcAsset *asset.Asset,
	internalKey *btcec.PublicKey,
	sibling *commitment.TapscriptPreimage) *proof.Proof {

	t.Helper()

	anchorAsset := htlcAsset.Copy()
	version := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(&version, anchorAsset)
	require.NoError(t, err)
	_, inclusion, err := tapCommitment.Proof(
		anchorAsset.TapCommitmentKey(), anchorAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)

	var siblingHash *chainhash.Hash
	if sibling != nil {
		siblingHash, err = sibling.TapHash()
		require.NoError(t, err)
	}
	pkScript, err := tapscript.PayToAddrScript(
		*internalKey, siblingHash, *tapCommitment,
	)
	require.NoError(t, err)

	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x51}},
	})
	anchorTx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: pkScript})
	anchorProof := &proof.Proof{
		AnchorTx: *anchorTx,
		Asset:    *anchorAsset,
		InclusionProof: proof.TaprootProof{
			InternalKey: internalKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof:              *inclusion,
				TapSiblingPreimage: sibling,
			},
		},
	}
	_, err = anchorProof.VerifyProofs()
	require.NoError(t, err)

	return anchorProof
}

// TestValidateProofBindsContract accepts only proofs whose anchor output is
// this contract's HTLC output. Each rejected proof is structurally valid and
// describes its complete anchor, so the commitment helpers alone accept it.
func TestValidateProofBindsContract(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	reverse := newWitnessFixtureForDirection(
		t, SuccessSequence, asset.V1, commitment.TapCommitmentV2, true,
	)
	internalKey, err := f.kit.GetAggregateKey()
	require.NoError(t, err)
	sibling, err := f.kit.GetSiblingPreimage()
	require.NoError(t, err)
	_, funderKey := scalarKey(t, 9)

	// Terms that differ only in their refund delay share the internal key.
	otherParams := kitParams(f.kit)
	otherParams.CsvExpiry++
	otherKit, err := NewSwapKit(AssetHTLCV0, otherParams)
	require.NoError(t, err)
	otherSibling, err := otherKit.GetSiblingPreimage()
	require.NoError(t, err)

	tests := []struct {
		name  string
		proof *proof.Proof
		err   string
	}{
		{
			name:  "contract output",
			proof: f.proof,
		},
		{
			name: "funder-only output",
			proof: anchorProofUnder(
				t, &f.proof.Asset, funderKey, nil,
			),
			err: "internal key does not match",
		},
		{
			name: "funder key with contract scripts",
			proof: anchorProofUnder(
				t, &f.proof.Asset, funderKey, &sibling,
			),
			err: "internal key does not match",
		},
		{
			name: "key path only",
			proof: anchorProofUnder(
				t, &f.proof.Asset, internalKey, nil,
			),
			err: "tapscript sibling does not match",
		},
		{
			name: "other refund delay",
			proof: anchorProofUnder(
				t, &f.proof.Asset, internalKey, &otherSibling,
			),
			err: "tapscript sibling does not match",
		},
		{
			name:  "reversed swap direction",
			proof: reverse.proof,
			err:   "tapscript sibling does not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := f.kit.ValidateProof(test.proof)
			pkScript, scriptErr := f.kit.GetPkScriptFromProof(
				test.proof,
			)
			if test.err == "" {
				require.NoError(t, err)
				require.NoError(t, scriptErr)
				outputs := test.proof.AnchorTx.TxOut
				index := test.proof.InclusionProof.OutputIndex
				require.Equal(t, outputs[index].PkScript, pkScript)

				return
			}

			require.ErrorContains(t, err, test.err)
			require.ErrorContains(t, scriptErr, test.err)
			require.Nil(t, pkScript)
			_, err = GenTaprootAssetRootFromProof(test.proof)
			require.NoError(t, err)
		})
	}

	// Honest funding layouts resolve to the contract output.
	for _, layout := range fundingLayouts() {
		t.Run("funded/"+layout.name, func(t *testing.T) {
			htlcProof := fundHtlcProof(t, f, layout)
			require.NoError(t, f.kit.ValidateProof(htlcProof))
		})
	}

	require.ErrorContains(
		t, f.kit.ValidateProof(nil), "asset proof is required",
	)
}

// TestValidateProofActivation accepts the funding proofs that Taproot Assets
// v0.8.5 creates on a network past activation. Each kind of missing evidence
// is rejected there, unless the proof claims a height before activation.
func TestValidateProofActivation(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	mainnet := mainnetKit(t, f.kit)

	// Issuance is exempt, and tapd's funding transfers carry the evidence.
	require.NoError(t, mainnet.ValidateProof(f.proof))
	for _, layout := range fundingLayouts() {
		htlcProof := fundHtlcProof(t, f, layout)
		require.NoError(t, mainnet.ValidateProof(htlcProof))
	}

	// Funding with change makes the HTLC a split asset, which proves its
	// transfer root through its split root proof.
	funding := fundHtlcProof(t, f, fundingLayout{
		name: "address with change", source: 1500,
	})
	require.True(t, funding.Asset.HasSplitCommitmentWitness())

	// strip returns a copy of the funding proof, whose split root proof
	// the mutation may change without affecting the original.
	strip := func(mutate func(*proof.Proof)) *proof.Proof {
		stripped := *funding
		splitRoot := *funding.SplitRootProof
		rootCommitment := *splitRoot.CommitmentProof
		splitRoot.CommitmentProof = &rootCommitment
		stripped.SplitRootProof = &splitRoot
		mutate(&stripped)

		return &stripped
	}
	const activation = proof.MainNetActivationHeight

	tests := []struct {
		name  string
		proof *proof.Proof
		err   error
	}{
		{
			name: "version zero",
			proof: strip(func(p *proof.Proof) {
				p.Version = proof.TransitionV0
			}),
			err: proof.ErrTransitionV1Required,
		},
		{
			name: "missing root locator",
			proof: strip(func(p *proof.Proof) {
				p.RootLocatorProof = nil
			}),
			err: proof.ErrMissingRootLocatorProof,
		},
		{
			name: "missing split root STXO proofs",
			proof: strip(func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.STXOProofs = nil
			}),
			err: proof.ErrMissingSplitSTXOProofs,
		},
		{
			name: "missing spender proofs",
			proof: strip(func(p *proof.Proof) {
				commitmentProof := p.SplitRootProof.CommitmentProof
				commitmentProof.SpenderProofs = nil
			}),
			err: proof.ErrMissingSpenderProofs,
		},
		{
			name: "claimed activation height",
			proof: strip(func(p *proof.Proof) {
				p.Version = proof.TransitionV0
				p.BlockHeight = activation
			}),
			err: proof.ErrTransitionV1Required,
		},
		{
			name: "claimed height before activation",
			proof: strip(func(p *proof.Proof) {
				p.Version = proof.TransitionV0
				p.BlockHeight = activation - 1
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Regtest schedules no activation height.
			require.NoError(t, f.kit.ValidateProof(test.proof))

			err := mainnet.ValidateProof(test.proof)
			if test.err == nil {
				require.NoError(t, err)

				return
			}
			require.ErrorIs(t, err, test.err)
		})
	}
}

// fileVerifier records proof file verification requests and returns a fixed
// tapd response.
type fileVerifier struct {
	response *taprpc.VerifyProofResponse
	err      error
	calls    int
	version  string
}

// GetInfo reports the proof verifier's configured backend version.
func (v *fileVerifier) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	version := v.version
	if version == "" {
		version = "0.8.6-alpha"
	}

	return &taprpc.GetInfoResponse{Version: version}, nil
}

// VerifyProof records the request and returns the configured response.
func (v *fileVerifier) VerifyProof(context.Context, *taprpc.ProofFile,
	...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	v.calls++

	return v.response, v.err
}

// tapdVerifier runs tapd's proof file verification with only chain lookups
// mocked; commitment and asset VM verification remain real.
type tapdVerifier struct{}

// GetInfo reports a supported backend for real proof-verification fixtures.
func (tapdVerifier) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	return &taprpc.GetInfoResponse{Version: "0.8.6-alpha"}, nil
}

// VerifyProof verifies the decoded file with the upstream proof verifier.
func (tapdVerifier) VerifyProof(ctx context.Context, req *taprpc.ProofFile,
	_ ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	file, err := proof.DecodeFile(req.RawProofFile)
	if err != nil {
		return nil, err
	}
	_, err = file.Verify(ctx, proof.MockVerifierCtx)

	return &taprpc.VerifyProofResponse{Valid: err == nil}, nil
}

// encodeHtlcProofFile serializes a proof history for the RPC boundary.
func encodeHtlcProofFile(t *testing.T,
	proofs ...proof.Proof) *taprpc.ProofFile {

	t.Helper()

	file, err := proof.NewFile(proof.V0, proofs...)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, file.Encode(&encoded))

	return &taprpc.ProofFile{RawProofFile: encoded.Bytes()}
}

// htlcIssuanceProof issues the fixture's asset directly into the contract's
// HTLC output, revealing the genesis that tapd verifies.
func htlcIssuanceProof(t *testing.T, f *witnessFixture) *proof.Proof {
	t.Helper()

	internalKey, err := f.kit.GetAggregateKey()
	require.NoError(t, err)
	sibling, err := f.kit.GetSiblingPreimage()
	require.NoError(t, err)
	issuance := anchorProofUnder(t, &f.proof.Asset, internalKey, &sibling)
	genesis := f.proof.Asset.Genesis
	issuance.AnchorTx.TxIn[0].PreviousOutPoint = genesis.FirstPrevOut
	issuance.PrevOut = genesis.FirstPrevOut
	issuance.GenesisReveal = &genesis

	return issuance
}

// htlcTransferProof spends the previous proof's asset into an output with the
// given internal key and tapscript sibling.
func htlcTransferProof(t *testing.T, prev *proof.Proof,
	internalKey *btcec.PublicKey,
	sibling *commitment.TapscriptPreimage) *proof.Proof {

	t.Helper()

	transferAsset := prev.Asset.Copy()
	transferAsset.PrevWitnesses = []asset.Witness{{
		PrevID: &asset.PrevID{
			OutPoint: prev.OutPoint(),
			ID:       transferAsset.ID(),
			ScriptKey: asset.ToSerialized(
				transferAsset.ScriptKey.PubKey,
			),
		},
		TxWitness: opTrueWitness(t),
	}}
	transfer := anchorProofUnder(t, transferAsset, internalKey, sibling)
	transfer.AnchorTx.TxIn[0].PreviousOutPoint = prev.OutPoint()
	transfer.PrevOut = prev.OutPoint()

	return transfer
}

// TestVerifyProofFile accepts complete HTLC histories that tapd verifies and
// that end in the contract's output. Histories without provenance, or with
// transfers that miss the activation evidence, never reach tapd.
func TestVerifyProofFile(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	internalKey, err := f.kit.GetAggregateKey()
	require.NoError(t, err)
	sibling, err := f.kit.GetSiblingPreimage()
	require.NoError(t, err)
	_, funderKey := scalarKey(t, 9)
	issuance := htlcIssuanceProof(t, f)
	transfer := htlcTransferProof(t, issuance, internalKey, &sibling)

	for _, history := range [][]proof.Proof{
		{*issuance}, {*issuance, *transfer},
	} {
		outpoint := history[len(history)-1].OutPoint()
		accepted, err := f.kit.VerifyProofFile(
			t.Context(), tapdVerifier{},
			encodeHtlcProofFile(t, history...), &outpoint,
		)
		require.NoError(t, err)
		require.Equal(t, outpoint, accepted.OutPoint())
	}

	// Structural checks must not replace tapd's verification.
	invalid := *transfer
	invalid.Asset = *transfer.Asset.Copy()
	invalid.Asset.PrevWitnesses[0].TxWitness = nil
	invalidOutpoint := invalid.OutPoint()
	_, err = f.kit.VerifyProofFile(
		t.Context(), tapdVerifier{},
		encodeHtlcProofFile(t, *issuance, invalid), &invalidOutpoint,
	)
	require.ErrorContains(t, err, "invalid HTLC proof file")

	history := encodeHtlcProofFile(t, *issuance, *transfer)
	ownership := *transfer
	ownership.ChallengeWitness = opTrueWitness(t)
	funderOnly := htlcTransferProof(t, issuance, funderKey, nil)
	deep := encodeHtlcProofFile(t, *issuance)
	for range policy.MaxProvenanceDepth + 1 {
		decoded, err := proof.DecodeFile(deep.RawProofFile)
		require.NoError(t, err)
		parent := *issuance
		parent.AdditionalInputs = []proof.File{*decoded}
		deep = encodeHtlcProofFile(t, parent)
	}
	oversized := &taprpc.ProofFile{
		RawProofFile: make([]byte, policy.MaxProvenanceBytes+1),
	}

	tests := []struct {
		name       string
		kit        *SwapKit
		file       *taprpc.ProofFile
		outpoint   wire.OutPoint
		verifier   *fileVerifier
		err        string
		errIs      error
		tapdCalled bool
	}{
		{
			name:     "ownership-only history",
			file:     encodeHtlcProofFile(t, ownership),
			outpoint: ownership.OutPoint(),
			err:      "ownership challenge",
		},
		{
			name:     "history without genesis",
			file:     encodeHtlcProofFile(t, *transfer),
			outpoint: transfer.OutPoint(),
			err:      "must start at genesis",
		},
		{
			name:     "deeply nested inputs",
			file:     deep,
			outpoint: issuance.OutPoint(),
			err:      "levels deep",
		},
		{
			name:     "activated transfer without evidence",
			kit:      mainnetKit(t, f.kit),
			file:     history,
			outpoint: transfer.OutPoint(),
			errIs:    proof.ErrTransitionV1Required,
		},
		{
			name:     "oversized file",
			file:     oversized,
			outpoint: transfer.OutPoint(),
			err:      "byte limit",
		},
		{
			name:     "tapd rejects the file",
			file:     history,
			outpoint: transfer.OutPoint(),
			verifier: &fileVerifier{
				response: &taprpc.VerifyProofResponse{},
			},
			err:        "invalid HTLC proof file",
			tapdCalled: true,
		},
		{
			name:       "tapd fails",
			file:       history,
			outpoint:   transfer.OutPoint(),
			verifier:   &fileVerifier{err: context.Canceled},
			err:        "unable to verify HTLC proof file",
			tapdCalled: true,
		},
		{
			name:       "other outpoint",
			file:       history,
			outpoint:   issuance.OutPoint(),
			err:        "outpoint mismatch",
			tapdCalled: true,
		},
		{
			name:       "funder-only terminal output",
			file:       encodeHtlcProofFile(t, *issuance, *funderOnly),
			outpoint:   funderOnly.OutPoint(),
			err:        "internal key does not match",
			tapdCalled: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kit := test.kit
			if kit == nil {
				kit = f.kit
			}
			verifier := test.verifier
			if verifier == nil {
				verifier = &fileVerifier{
					response: &taprpc.VerifyProofResponse{
						Valid: true,
					},
				}
			}
			outpoint := test.outpoint
			accepted, err := kit.VerifyProofFile(
				t.Context(), verifier, test.file, &outpoint,
			)
			require.Nil(t, accepted)
			if test.errIs != nil {
				require.ErrorIs(t, err, test.errIs)
			} else {
				require.ErrorContains(t, err, test.err)
			}
			require.Equal(t, test.tapdCalled, verifier.calls > 0)
		})
	}

	outpoint := issuance.OutPoint()
	file := encodeHtlcProofFile(t, *issuance)
	verifier := &fileVerifier{}
	_, err = (*SwapKit)(nil).VerifyProofFile(
		t.Context(), verifier, file, &outpoint,
	)
	require.ErrorContains(t, err, "swap kit is required")
	_, err = f.kit.VerifyProofFile(t.Context(), nil, file, &outpoint)
	require.ErrorContains(t, err, "proof verifier is required")
	_, err = f.kit.VerifyProofFile(t.Context(), verifier, nil, &outpoint)
	require.ErrorContains(t, err, "proof file is required")
	_, err = f.kit.VerifyProofFile(
		t.Context(), verifier, &taprpc.ProofFile{}, &outpoint,
	)
	require.ErrorContains(t, err, "proof file is required")
	_, err = f.kit.VerifyProofFile(t.Context(), verifier, file, nil)
	require.ErrorContains(t, err, "expected HTLC outpoint is required")
	require.Zero(t, verifier.calls)
}

// TestVerifyProofFileTapdVersion checks that an older backend cannot accept
// untrusted proof histories, while a supported verifier accepts the same file.
func TestVerifyProofFileTapdVersion(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	issuance := htlcIssuanceProof(t, f)
	outpoint := issuance.OutPoint()
	history := encodeHtlcProofFile(t, *issuance)
	for _, version := range []string{"0.8.5-alpha", "0.8.6-alpha"} {
		t.Run(version, func(t *testing.T) {
			verifier := &fileVerifier{
				version: version,
				response: &taprpc.VerifyProofResponse{
					Valid: true,
				},
			}
			accepted, err := f.kit.VerifyProofFile(
				t.Context(), verifier, history, &outpoint,
			)
			if version == "0.8.5-alpha" {
				require.ErrorIs(
					t, err, policy.ErrTapdVersionUnsupported,
				)
				require.Nil(t, accepted)
				require.Zero(t, verifier.calls)
			} else {
				require.NoError(t, err)
				require.Equal(t, outpoint, accepted.OutPoint())
				require.Equal(t, 1, verifier.calls)
			}
		})
	}
}
