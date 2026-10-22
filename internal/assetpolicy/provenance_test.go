package policy

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/mssmt"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// testKey derives a deterministic test key from a one-byte scalar.
func testKey(scalar byte) *btcec.PublicKey {
	keyBytes := make([]byte, 32)
	keyBytes[31] = scalar
	_, publicKey := btcec.PrivKeyFromBytes(keyBytes)

	return publicKey
}

// issuanceProof returns an encodable issuance proof. The provenance walk only
// decodes proofs, so it need not pass verification.
func issuanceProof(t *testing.T) proof.Proof {
	t.Helper()

	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{0x61}, Index: 1},
		Tag:          "provenance policy test",
		Type:         asset.Normal,
	}
	issued, err := asset.New(
		genesis, 1000, 0, 0, asset.NewScriptKey(testKey(1)), nil,
		asset.WithAssetVersion(asset.V1),
	)
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(&version, issued)
	require.NoError(t, err)
	_, inclusion, err := tapCommitment.Proof(
		issued.TapCommitmentKey(), issued.AssetCommitmentKey(),
	)
	require.NoError(t, err)

	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{PreviousOutPoint: genesis.FirstPrevOut})
	anchorTx.AddTxOut(&wire.TxOut{
		Value: 1000, PkScript: []byte{txscript.OP_TRUE},
	})

	return proof.Proof{
		PrevOut:       genesis.FirstPrevOut,
		AnchorTx:      *anchorTx,
		Asset:         *issued,
		GenesisReveal: &genesis,
		InclusionProof: proof.TaprootProof{
			InternalKey: testKey(2),
			CommitmentProof: &proof.CommitmentProof{
				Proof: *inclusion,
			},
		},
	}
}

// transferProof returns an encodable proof that spends the issued asset and
// carries none of the evidence the activation rules require.
func transferProof(t *testing.T, issuance proof.Proof) proof.Proof {
	t.Helper()

	transfer := issuance
	transfer.Asset = *issuance.Asset.Copy()
	transfer.Asset.PrevWitnesses = []asset.Witness{{
		PrevID: &asset.PrevID{
			OutPoint: issuance.OutPoint(),
			ID:       issuance.Asset.ID(),
			ScriptKey: asset.ToSerialized(
				issuance.Asset.ScriptKey.PubKey,
			),
		},
		TxWitness: wire.TxWitness{{0x01}},
	}}
	transfer.GenesisReveal = nil
	transfer.PrevOut = issuance.OutPoint()

	return transfer
}

// newFile encodes the proofs into a proof file.
func newFile(t *testing.T, proofs ...proof.Proof) *proof.File {
	t.Helper()

	file, err := proof.NewFile(proof.V0, proofs...)
	require.NoError(t, err)

	return file
}

// nest returns a file whose single proof carries file as an additional input.
func nest(t *testing.T, issuance proof.Proof, file *proof.File) *proof.File {
	t.Helper()

	parent := issuance
	parent.AdditionalInputs = []proof.File{*file}

	return newFile(t, parent)
}

// nested returns an issuance file wrapped depth times in additional inputs.
func nested(t *testing.T, issuance proof.Proof, depth int) *proof.File {
	t.Helper()

	file := newFile(t, issuance)
	for range depth {
		file = nest(t, issuance, file)
	}

	return file
}

// TestActivationHeight uses the scheduled height of each network, and tapd's
// process-wide default for networks without one.
func TestActivationHeight(t *testing.T) {
	scheduled := []struct {
		params *address.ChainParams
		height uint32
	}{
		{&address.MainNetTap, proof.MainNetActivationHeight},
		{&address.TestNet3Tap, proof.TestNet3ActivationHeight},
		{&address.TestNet4Tap, proof.TestNet4ActivationHeight},
		{&address.SigNetTap, proof.SigNetActivationHeight},
	}
	for _, network := range scheduled {
		height, activated := ActivationHeight(network.params)
		require.True(t, activated)
		require.Equal(t, network.height, height)
	}
	for _, params := range []*address.ChainParams{
		&address.RegressionNetTap, &address.SimNetTap, nil,
	} {
		_, activated := ActivationHeight(params)
		require.False(t, activated)
	}

	// Regtest follows the default, while scheduled heights take precedence.
	proof.SetDefaultActivationHeight(
		proof.NetworkActivationHeight(chaincfg.TestNet4Params.Name),
	)
	t.Cleanup(func() {
		proof.SetDefaultActivationHeight(proof.NetworkActivationHeight(
			chaincfg.RegressionNetParams.Name,
		))
	})
	height, activated := ActivationHeight(&address.RegressionNetTap)
	require.True(t, activated)
	require.Equal(t, proof.TestNet4ActivationHeight, height)
	height, activated = ActivationHeight(&address.MainNetTap)
	require.True(t, activated)
	require.Equal(t, proof.MainNetActivationHeight, height)
}

// TestValidateActivation mirrors each Taproot Assets v0.8.5 activation rule
// for root transfers, split roots and split assets.
func TestValidateActivation(t *testing.T) {
	const activationHeight = proof.MainNetActivationHeight

	issuance := issuanceProof(t)
	spenders := map[asset.SerializedKey]commitment.Proof{{0x02}: {}}
	stxos := map[asset.SerializedKey]commitment.Proof{{0x03}: {}}

	// rootTransfer returns a version one root transfer proof with spender
	// proofs, which mutate may strip.
	rootTransfer := func(mutate func(*proof.Proof)) *proof.Proof {
		p := transferProof(t, issuance)
		p.Version = proof.TransitionV1
		p.InclusionProof.CommitmentProof = &proof.CommitmentProof{
			SpenderProofs: spenders,
		}
		if mutate != nil {
			mutate(&p)
		}

		return &p
	}

	// splitRoot returns a split root asset proof with its root locator.
	splitRoot := func(mutate func(*proof.Proof)) *proof.Proof {
		return rootTransfer(func(p *proof.Proof) {
			p.Asset.SplitCommitmentRoot = mssmt.NewComputedNode(
				mssmt.NodeHash{0x04}, 1000,
			)
			p.RootLocatorProof = &mssmt.Proof{}
			if mutate != nil {
				mutate(p)
			}
		})
	}

	// splitAsset returns a split asset proof whose split root proof
	// carries its root's STXO and spender proofs.
	splitAsset := func(mutate func(*proof.Proof)) *proof.Proof {
		p := transferProof(t, issuance)
		p.Version = proof.TransitionV1
		p.Asset.PrevWitnesses = []asset.Witness{{
			PrevID: &asset.ZeroPrevID,
			SplitCommitment: &asset.SplitCommitment{
				RootAsset: *issuance.Asset.Copy(),
			},
		}}
		p.RootLocatorProof = &mssmt.Proof{}
		p.SplitRootProof = &proof.TaprootProof{
			CommitmentProof: &proof.CommitmentProof{
				STXOProofs:    stxos,
				SpenderProofs: spenders,
			},
		}
		if mutate != nil {
			mutate(&p)
		}

		return &p
	}

	tests := []struct {
		name  string
		proof *proof.Proof
		err   error
	}{
		{
			name:  "issuance",
			proof: &issuance,
		},
		{
			name:  "complete root transfer",
			proof: rootTransfer(nil),
		},
		{
			name: "version zero transfer",
			proof: rootTransfer(func(p *proof.Proof) {
				p.Version = proof.TransitionV0
			}),
			err: proof.ErrTransitionV1Required,
		},
		{
			name: "transfer without spender proofs",
			proof: rootTransfer(func(p *proof.Proof) {
				p.InclusionProof.CommitmentProof = nil
			}),
			err: proof.ErrMissingSpenderProofs,
		},
		{
			name:  "complete split root",
			proof: splitRoot(nil),
		},
		{
			name: "split root without root locator",
			proof: splitRoot(func(p *proof.Proof) {
				p.RootLocatorProof = nil
			}),
			err: proof.ErrMissingRootLocatorProof,
		},
		{
			name:  "complete split asset",
			proof: splitAsset(nil),
		},
		{
			name: "split asset without root locator",
			proof: splitAsset(func(p *proof.Proof) {
				p.RootLocatorProof = nil
			}),
			err: proof.ErrMissingRootLocatorProof,
		},
		{
			name: "split asset without split root STXO proofs",
			proof: splitAsset(func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.STXOProofs = nil
			}),
			err: proof.ErrMissingSplitSTXOProofs,
		},
		{
			name: "split asset without split root spender proofs",
			proof: splitAsset(func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.SpenderProofs = nil
			}),
			err: proof.ErrMissingSpenderProofs,
		},
		{
			name: "split asset without split root proof",
			proof: splitAsset(func(p *proof.Proof) {
				p.SplitRootProof = nil
			}),
			err: proof.ErrMissingSplitSTXOProofs,
		},
		{
			name: "claimed height before activation",
			proof: rootTransfer(func(p *proof.Proof) {
				p.Version = proof.TransitionV0
				p.BlockHeight = activationHeight - 1
			}),
		},
		{
			name: "claimed activation height",
			proof: rootTransfer(func(p *proof.Proof) {
				p.Version = proof.TransitionV0
				p.BlockHeight = activationHeight
			}),
			err: proof.ErrTransitionV1Required,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateActivation(test.proof, activationHeight)
			if test.err == nil {
				require.NoError(t, err)

				return
			}
			require.ErrorIs(t, err, test.err)
		})
	}

	require.ErrorContains(
		t, ValidateActivation(nil, activationHeight), "proof is required",
	)
}

// TestValidateProvenance rejects histories without provenance anywhere in the
// input tree, and applies the activation rules of the given network.
func TestValidateProvenance(t *testing.T) {
	issuance := issuanceProof(t)
	transfer := transferProof(t, issuance)
	ownership := transfer
	ownership.ChallengeWitness = wire.TxWitness{{0x01}}
	preActivation := transfer
	preActivation.BlockHeight = proof.MainNetActivationHeight - 1

	tests := []struct {
		name   string
		file   *proof.File
		params *address.ChainParams
		err    string
		errIs  error
	}{
		{
			name: "issuance",
			file: newFile(t, issuance),
		},
		{
			name: "transfer history",
			file: newFile(t, issuance, transfer),
		},
		{
			name: "nested history",
			file: nest(t, issuance, newFile(t, issuance, transfer)),
		},
		{
			name: "empty history",
			file: newFile(t),
			err:  "history is empty",
		},
		{
			name: "ownership proof",
			file: newFile(t, ownership),
			err:  testOwnershipChallenge,
		},
		{
			name: "later ownership proof",
			file: newFile(t, issuance, ownership),
			err:  testOwnershipChallenge,
		},
		{
			name: "missing genesis",
			file: newFile(t, transfer),
			err:  "must start at genesis",
		},
		{
			name: "nested ownership proof",
			file: nest(t, issuance, nest(t, issuance,
				newFile(t, ownership))),
			err: testOwnershipChallenge,
		},
		{
			name: "nested input missing genesis",
			file: nest(t, issuance, newFile(t, transfer)),
			err:  "must start at genesis",
		},
		{
			name: "nested empty input",
			file: nest(t, issuance, newFile(t)),
			err:  "history is empty",
		},
		{
			name:   "activated issuance",
			file:   newFile(t, issuance),
			params: &address.MainNetTap,
		},
		{
			name:   "activated transfer without evidence",
			file:   newFile(t, issuance, transfer),
			params: &address.MainNetTap,
			errIs:  proof.ErrTransitionV1Required,
		},
		{
			name: "activated nested transfer without evidence",
			file: nest(t, issuance,
				newFile(t, issuance, transfer)),
			params: &address.MainNetTap,
			errIs:  proof.ErrTransitionV1Required,
		},
		{
			name:   "transfer claimed before activation",
			file:   newFile(t, issuance, preActivation),
			params: &address.MainNetTap,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := test.params
			if params == nil {
				params = &address.RegressionNetTap
			}
			err := ValidateProvenance(t.Context(), test.file, params)
			switch {
			case test.err != "":
				require.ErrorContains(t, err, test.err)

			case test.errIs != nil:
				require.ErrorIs(t, err, test.errIs)

			default:
				require.NoError(t, err)
			}
		})
	}

	file := newFile(t, issuance)
	file.Version = ProofFileVersion + 1
	require.ErrorContains(t, ValidateProvenance(
		t.Context(), file, &address.RegressionNetTap,
	), "unsupported proof file version")
	require.ErrorContains(t, ValidateProvenance(
		t.Context(), nil, &address.RegressionNetTap,
	), "proof file is required")
}

// TestValidateProvenanceBounds stops nested input histories at the depth and
// decoding budget limits, which charge every nested copy that decoding makes.
func TestValidateProvenanceBounds(t *testing.T) {
	issuance := issuanceProof(t)
	params := &address.RegressionNetTap

	require.NoError(t, ValidateProvenance(
		t.Context(), nested(t, issuance, MaxProvenanceDepth), params,
	))
	require.ErrorContains(t, ValidateProvenance(
		t.Context(), nested(t, issuance, MaxProvenanceDepth+1), params,
	), "levels deep")

	// The terminal proof alone fits the budget, but the copy of its nested
	// input that decoding makes does not.
	file := nested(t, issuance, 1)
	terminal, err := file.RawProofAt(0)
	require.NoError(t, err)
	require.ErrorContains(t, validateProvenance(
		t.Context(), file, params, MaxProvenanceDepth, len(terminal)+1,
	), "decoding budget")
	require.NoError(t, validateProvenance(
		t.Context(), file, params, MaxProvenanceDepth, 2*len(terminal),
	))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(
		t, ValidateProvenance(ctx, file, params), context.Canceled,
	)
}
