package tapsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestAssetProofPathRequiresActivationEvidence rejects incomplete new
// transitions even when a caller supplies a pre-activation confirmed base.
func TestAssetProofPathRequiresActivationEvidence(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	split := newAssetProofPathSplitTransition(
		t, fixture.baseProof, testPrivateKey(t, 1),
	)
	tests := []struct {
		name       string
		transition *proof.Proof
		mutate     func(*proof.Proof)
		wantErr    error
	}{
		{
			name:       "spender proofs",
			transition: fixture.transition,
			mutate: func(p *proof.Proof) {
				p.InclusionProof.CommitmentProof.
					SpenderProofs = nil
			},
			wantErr: proof.ErrMissingSpenderProofs,
		},
		{
			name:       "root locator",
			transition: split,
			mutate: func(p *proof.Proof) {
				p.RootLocatorProof = nil
			},
			wantErr: proof.ErrMissingRootLocatorProof,
		},
		{
			name:       "split root STXOs",
			transition: split,
			mutate: func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.
					STXOProofs = nil
			},
			wantErr: proof.ErrMissingSplitSTXOProofs,
		},
		{
			name:       "split root spenders",
			transition: split,
			mutate: func(p *proof.Proof) {
				p.SplitRootProof.CommitmentProof.
					SpenderProofs = nil
			},
			wantErr: proof.ErrMissingSpenderProofs,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := test.transition.Bytes()
			require.NoError(t, err)
			transition, err := proof.Decode(raw)
			require.NoError(t, err)
			test.mutate(transition)
			raw, err = transition.Bytes()
			require.NoError(t, err)
			path := &AssetProofPath{
				ConfirmedBaseProof: fixture.baseProofFile,
				Steps: []AssetProofPathStep{{
					TransitionProof: raw,
				}},
			}
			err = path.Validate()
			require.ErrorIs(t, err, ErrAssetProofPathInvalid)
			require.ErrorIs(t, err, test.wantErr)
		})
	}
}

// TestAssetProofPathVerifiesSpenders rejects a present but invalid spender
// inclusion proof using the native proof verifier.
func TestAssetProofPathVerifiesSpenders(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	commitmentProof := fixture.transition.InclusionProof.CommitmentProof
	for key := range commitmentProof.SpenderProofs {
		for _, stxoProof := range commitmentProof.STXOProofs {
			commitmentProof.SpenderProofs[key] = stxoProof
		}
	}
	transition, err := fixture.transition.Bytes()
	require.NoError(t, err)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transition,
		}},
	}
	require.NoError(t, path.Validate())
	_, err = path.Verify(context.Background(), &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	})
	require.ErrorIs(t, err, commitment.ErrInvalidTaprootProof)
}

// TestAssetProofPathSpendsHistoricalV0Base keeps activation policy for the
// confirmed history with the confirmed verifier, while checking each new step.
func TestAssetProofPathSpendsHistoricalV0Base(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	legacy := fixture.transition
	legacy.Version = proof.TransitionV0
	legacy.InclusionProof.CommitmentProof.SpenderProofs = nil
	base, err := proof.NewFile(proof.V0, *fixture.baseProof, *legacy)
	require.NoError(t, err)
	var encodedBase bytes.Buffer
	require.NoError(t, base.Encode(&encodedBase))

	transition := newAssetProofPathTransition(
		t, legacy, testPrivateKey(t, 3), testPrivateKey(t, 4),
	)
	raw, err := transition.Bytes()
	require.NoError(t, err)
	path := &AssetProofPath{
		ConfirmedBaseProof: encodedBase.Bytes(),
		Steps: []AssetProofPathStep{{
			TransitionProof: raw,
		}},
	}
	_, err = path.Verify(context.Background(), &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	})
	require.NoError(t, err)
}

// TestAssetProofPathRoundTripAndVerify exercises the persistence boundary and
// the real local Taproot Assets proof verifier as one flow.
func TestAssetProofPathRoundTripAndVerify(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: fixture.transitionProof,
		}},
	}

	require.NoError(t, path.Validate())
	contentID, err := path.ContentID()
	require.NoError(t, err)
	require.NotEqual(t, Hash{}, contentID)

	encoded, err := path.MarshalBinary()
	require.NoError(t, err)
	encodedAgain, err := path.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, encoded, encodedAgain)

	var decoded AssetProofPath
	require.NoError(t, decoded.UnmarshalBinary(encoded))
	require.Equal(t, path, &decoded)
	decodedID, err := decoded.ContentID()
	require.NoError(t, err)
	require.Equal(t, contentID, decodedID)

	stepSummary, err := decoded.Steps[0].Summary()
	require.NoError(t, err)
	require.Equal(t, fixture.baseProof.OutPoint().String(),
		stepSummary.PreviousAnchorOutpoint.String())
	require.Equal(t, fixture.transition.OutPoint().String(),
		stepSummary.AnchorOutpoint.String())
	require.Equal(t, int64(330), stepSummary.AnchorValueSat)
	require.Equal(t, uint64(100), stepSummary.Amount)
	require.False(t, stepSummary.SplitAsset)

	verifier := &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	}
	summary, err := decoded.Verify(context.Background(), verifier)
	require.NoError(t, err)
	require.Equal(t, uint16(1), summary.Depth)
	require.Equal(t, contentID, summary.ContentID)
	require.Equal(t, stepSummary.AssetRef, summary.AssetRef)
	require.Equal(t, stepSummary.IssuanceID, summary.IssuanceID)
	require.Equal(t, stepSummary.ScriptKey, summary.ScriptKey)
	require.Equal(t, stepSummary.AnchorOutpoint, summary.AnchorOutpoint)
	require.Equal(t, stepSummary.AnchorValueSat, summary.AnchorValueSat)
	require.Equal(t, 1, verifier.calls)
	require.Equal(t, 1, verifier.unconfirmedCalls)
	require.Len(t, verifier.unconfirmedTransitions, 1)
	attestation := verifier.unconfirmedTransitions[0]
	require.Equal(t, uint16(0), attestation.StepIndex)
	require.Equal(t, stepSummary.PreviousAnchorOutpoint,
		attestation.PreviousAnchorOutpoint)
	require.Equal(t, []Outpoint{attestation.PreviousAnchorOutpoint},
		attestation.PreviousAnchorOutpoints)
	require.Equal(t, stepSummary.AnchorOutpoint, attestation.AnchorOutpoint)
	var anchorBytes bytes.Buffer
	require.NoError(t, fixture.transition.AnchorTx.Serialize(&anchorBytes))
	require.Equal(t, anchorBytes.Bytes(), attestation.AnchorTransaction)

	clone := decoded.Clone()
	require.Equal(t, &decoded, clone)
	clone.ConfirmedBaseProof[0] ^= 1
	clone.Steps[0].TransitionProof[0] ^= 1
	require.NotEqual(t, clone.ConfirmedBaseProof, decoded.ConfirmedBaseProof)
	require.NotEqual(t, clone.Steps[0].TransitionProof,
		decoded.Steps[0].TransitionProof)
	var nilPath *AssetProofPath
	require.Nil(t, nilPath.Clone())
}

// TestAssetProofPathAcceptsSplitSteps verifies the Ark tree case where the
// selected edge is a non-root output of a valid asset split.
func TestAssetProofPathAcceptsSplitSteps(t *testing.T) {
	t.Parallel()

	baseProofFile, baseProof, senderKey := newAssetProofPathBase(t)
	splitTransition := newAssetProofPathSplitTransition(
		t, baseProof, senderKey,
	)
	transitionProof, err := splitTransition.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transitionProof,
		}},
	}
	require.NoError(t, path.Validate())

	stepSummary, err := path.Steps[0].Summary()
	require.NoError(t, err)
	require.True(t, stepSummary.SplitAsset)
	require.Equal(t, uint64(40), stepSummary.Amount)
	require.Equal(t, int64(330), stepSummary.AnchorValueSat)

	summary, err := path.Verify(
		context.Background(), &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(40), summary.Amount)
	require.Equal(t, int64(330), summary.AnchorValueSat)
	require.Equal(t, splitTransition.OutPoint().String(),
		summary.AnchorOutpoint.String())

	incompleteRootWitness := mutateAssetProofPathTransition(
		t, transitionProof, func(p *proof.Proof) {
			rootAsset := &p.Asset.PrevWitnesses[0].
				SplitCommitment.RootAsset
			rootAsset.PrevWitnesses[0].TxWitness = nil
		},
	)
	incompletePath := path.Clone()
	incompletePath.Steps[0].TransitionProof = incompleteRootWitness
	require.ErrorContains(
		t, incompletePath.Validate(), "asset witness 0 is incomplete",
	)
}

func TestAssetProofPathMultiHopFromSplitChild(t *testing.T) {
	t.Parallel()

	baseProofFile, baseProof, senderKey := newAssetProofPathBase(t)
	splitTransition := newAssetProofPathSplitTransition(
		t, baseProof, senderKey,
	)
	// newAssetProofPathSplitTransition selects the 40-unit output locked to
	// test key 5. Spend that child again to prove a real two-hop chain.
	secondTransition := newAssetProofPathTransition(
		t, splitTransition, testPrivateKey(t, 5), testPrivateKey(t, 19),
	)
	splitBytes, err := splitTransition.Bytes()
	require.NoError(t, err)
	secondBytes, err := secondTransition.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: baseProofFile,
		Steps: []AssetProofPathStep{
			{TransitionProof: splitBytes},
			{TransitionProof: secondBytes},
		},
	}
	verifier := &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	}
	summary, err := path.Verify(context.Background(), verifier)
	require.NoError(t, err)
	require.Equal(t, uint16(2), summary.Depth)
	require.Equal(t, uint64(40), summary.Amount)
	require.Equal(t, outpointFromWire(secondTransition.OutPoint()),
		summary.AnchorOutpoint)
	require.Equal(t, 2, verifier.unconfirmedCalls)
	require.Equal(t, uint16(0),
		verifier.unconfirmedTransitions[0].StepIndex)
	require.Equal(t, uint16(1),
		verifier.unconfirmedTransitions[1].StepIndex)
}

// TestAssetProofPathAllowsBitcoinOnlyInputs verifies that connector and
// funding inputs don't become false asset predecessors.
func TestAssetProofPathAllowsBitcoinOnlyInputs(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	transitionProof := mutateAssetProofPathTransition(
		t, fixture.transitionProof, func(p *proof.Proof) {
			var connectorHash chainhash.Hash
			connectorHash[0] = 9
			p.AnchorTx.TxIn = append(p.AnchorTx.TxIn, &wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  connectorHash,
					Index: 2,
				},
			})
		},
	)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transitionProof,
		}},
	}
	require.NoError(t, path.Validate())

	summary, err := path.Verify(
		context.Background(), &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint16(1), summary.Depth)
}

// TestAssetProofPathRejectsHiddenCoAnchoredAsset proves that a valid
// inclusion proof isn't enough: the selected output must reconstruct exactly
// from the tracked asset and every disclosed alternate leaf.
func TestAssetProofPathRejectsHiddenCoAnchoredAsset(t *testing.T) {
	t.Parallel()

	baseProofFile, baseProof, senderKey := newAssetProofPathBase(t)
	transition := newAssetProofPathTransitionWithHiddenAsset(
		t, baseProof, senderKey, testPrivateKey(t, 3), true,
	)
	transitionProof, err := transition.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transitionProof,
		}},
	}
	require.NoError(t, path.Validate())

	_, err = path.Verify(
		context.Background(), &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
	)
	require.ErrorIs(t, err, ErrAssetProofPathPassiveAssets)
	require.ErrorContains(t, err, "selected output is not an isolated")
}

// TestAssetProofPathRejectsImmutableIdentityMutation proves that a signed,
// commitment-valid child cannot relabel the confirmed asset's immutable
// genesis or group parameters. In particular, native transition verification
// accepts several of these mutations because the witness PrevID still names
// the previous genesis ID.
func TestAssetProofPathRejectsImmutableIdentityMutation(t *testing.T) {
	t.Parallel()

	baseProofFile, baseProof, senderKey := newGroupedAssetProofPathBase(t)
	verifier := &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	}
	nativeContext := proof.VerifierCtx{
		GroupVerifier: func(*btcec.PublicKey) error { return nil },
	}
	validTransition := newAssetProofPathTransition(
		t, baseProof, senderKey, testPrivateKey(t, 3),
	)
	validTransitionBytes, err := validTransition.Bytes()
	require.NoError(t, err)
	validPath := &AssetProofPath{
		ConfirmedBaseProof: baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: validTransitionBytes,
		}},
	}
	_, err = validPath.Verify(context.Background(), verifier)
	require.NoError(t, err)

	testCases := []struct {
		name                 string
		mutate               func(*asset.Asset)
		errMsg               string
		nativeVerifierReject bool
	}{
		{
			name: "genesis",
			mutate: func(a *asset.Asset) {
				a.Genesis.Tag += "-mutated"
			},
			errMsg:               "selected asset genesis changed",
			nativeVerifierReject: true,
		},
		{
			name: "type",
			mutate: func(a *asset.Asset) {
				a.Genesis.Type = asset.Collectible
			},
			errMsg: "selected asset type changed",
		},
		{
			name: "group public key",
			mutate: func(a *asset.Asset) {
				require.NotNil(t, a.GroupKey)
				a.GroupKey.GroupPubKey = *testPrivateKey(t, 10).PubKey()
			},
			errMsg:               "selected asset group key changed",
			nativeVerifierReject: true,
		},
		{
			name: "group removed",
			mutate: func(a *asset.Asset) {
				a.GroupKey = nil
			},
			errMsg:               "selected asset group key changed",
			nativeVerifierReject: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			transition := newAssetProofPathTransitionWithMutator(
				t, baseProof, senderKey, testPrivateKey(t, 3), false,
				testCase.mutate,
			)
			if testCase.nativeVerifierReject {
				_, err := transition.Verify(
					context.Background(), &proof.AssetSnapshot{
						Asset:    baseProof.Asset.Copy(),
						OutPoint: baseProof.OutPoint(),
					}, assetProofPathChainLookup{}, nativeContext,
					proof.WithSkipChainVerification(),
				)
				require.ErrorContains(
					t, err, "asset id mismatch",
				)
			}

			transitionBytes, err := transition.Bytes()
			require.NoError(t, err)
			path := &AssetProofPath{
				ConfirmedBaseProof: baseProofFile,
				Steps: []AssetProofPathStep{{
					TransitionProof: transitionBytes,
				}},
			}
			require.NoError(t, path.Validate())

			_, err = path.Verify(context.Background(), verifier)
			require.ErrorIs(t, err, ErrAssetProofPathInvalid)
			require.ErrorContains(t, err, testCase.errMsg)
		})
	}
}

// TestAssetProofPathRejectsSplitRootIdentityMutation applies the same
// continuity rule to the complete root asset embedded in a selected split
// leaf. The selected leaf alone is not sufficient to authenticate that root.
func TestAssetProofPathRejectsSplitRootIdentityMutation(t *testing.T) {
	t.Parallel()

	baseProofFile, baseProof, senderKey := newGroupedAssetProofPathBase(t)
	transition := newAssetProofPathSplitTransition(t, baseProof, senderKey)
	transitionBytes, err := transition.Bytes()
	require.NoError(t, err)
	validPath := &AssetProofPath{
		ConfirmedBaseProof: baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transitionBytes,
		}},
	}
	_, err = validPath.Verify(
		context.Background(), &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
	)
	require.NoError(t, err)

	testCases := []struct {
		name   string
		mutate func(*asset.Asset)
		errMsg string
	}{
		{
			name: "genesis",
			mutate: func(root *asset.Asset) {
				root.Genesis.Tag += "-mutated"
			},
			errMsg: "split root asset genesis changed",
		},
		{
			name: "type",
			mutate: func(root *asset.Asset) {
				root.Genesis.Type = asset.Collectible
			},
			errMsg: "split root asset type changed",
		},
		{
			name: "group",
			mutate: func(root *asset.Asset) {
				require.NotNil(t, root.GroupKey)
				root.GroupKey.GroupPubKey = *testPrivateKey(t, 11).PubKey()
			},
			errMsg: "split root asset group key changed",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := mutateAssetProofPathTransition(
				t, transitionBytes, func(p *proof.Proof) {
					root := &p.Asset.PrevWitnesses[0].
						SplitCommitment.RootAsset
					testCase.mutate(root)
				},
			)
			path := &AssetProofPath{
				ConfirmedBaseProof: baseProofFile,
				Steps: []AssetProofPathStep{{
					TransitionProof: mutated,
				}},
			}
			require.NoError(t, path.Validate())

			_, err := path.Verify(
				context.Background(), &testConfirmedProofVerifier{
					result: &ConfirmedProofVerification{
						AnchorAssetInventoryComplete: true,
					},
				},
			)
			require.ErrorIs(t, err, ErrAssetProofPathInvalid)
			require.ErrorContains(t, err, testCase.errMsg)
		})
	}
}

// TestAssetProofPathRejectsUnsafeTransitions checks the policy constraints
// that keep an unconfirmed path independently verifiable.
func TestAssetProofPathRejectsUnsafeTransitions(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	baseFile := func(t *testing.T) proof.File {
		var file proof.File
		require.NoError(t, file.Decode(bytes.NewReader(
			fixture.baseProofFile,
		)))
		return file
	}

	testCases := []struct {
		name   string
		mutate func(*proof.Proof)
		errMsg string
	}{
		{
			name: "v0 transition proof",
			mutate: func(p *proof.Proof) {
				p.Version = proof.TransitionV0
			},
			errMsg: "transition proof must be v1",
		},
		{
			name: "v0 asset",
			mutate: func(p *proof.Proof) {
				p.Asset.Version = asset.V0
			},
			errMsg: "transition asset must be v1",
		},
		{
			name: "absolute timelock",
			mutate: func(p *proof.Proof) {
				p.Asset.LockTime = 500
			},
			errMsg: "asset timelocks are unsupported",
		},
		{
			name: "relative timelock",
			mutate: func(p *proof.Proof) {
				p.Asset.RelativeLockTime = 3
			},
			errMsg: "asset timelocks are unsupported",
		},
		{
			name: "missing witness",
			mutate: func(p *proof.Proof) {
				p.Asset.PrevWitnesses[0].TxWitness = nil
			},
			errMsg: "asset witness 0 is incomplete",
		},
		{
			name: "additional asset input path",
			mutate: func(p *proof.Proof) {
				file := baseFile(t)
				p.AdditionalInputs = []proof.File{file}
			},
			errMsg: "asset merges and additional input paths",
		},
		{
			name: "duplicate asset predecessor",
			mutate: func(p *proof.Proof) {
				p.AnchorTx.TxIn = append(
					p.AnchorTx.TxIn, &wire.TxIn{
						PreviousOutPoint: p.PrevOut,
					},
				)
			},
			errMsg: "asset predecessor exactly once",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			step := mutateAssetProofPathTransition(
				t, fixture.transitionProof, testCase.mutate,
			)
			path := &AssetProofPath{
				ConfirmedBaseProof: fixture.baseProofFile,
				Steps: []AssetProofPathStep{{
					TransitionProof: step,
				}},
			}

			err := path.Validate()
			require.ErrorContains(t, err, testCase.errMsg)
		})
	}
}

// TestAssetProofPathVerifierFailsClosed covers the external trust boundary
// and verifies that witness tampering is still detected locally.
func TestAssetProofPathVerifierFailsClosed(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: fixture.transitionProof,
		}},
	}

	testCases := []struct {
		name     string
		verifier ConfirmedProofVerifier
		target   error
	}{
		{
			name: "nil verifier",
		},
		{
			name:     "missing unconfirmed anchor verifier",
			verifier: confirmedOnlyProofVerifier{},
			target:   ErrAssetProofPathUnconfirmedAnchor,
		},
		{
			name: "unconfirmed anchor rejection",
			verifier: &testConfirmedProofVerifier{
				result: &ConfirmedProofVerification{
					AnchorAssetInventoryComplete: true,
				},
				unconfirmedErr: errors.New("unsigned Ark input"),
			},
			target: ErrAssetProofPathUnconfirmedAnchor,
		},
		{
			name: "unknown inventory",
			verifier: &testConfirmedProofVerifier{
				result: &ConfirmedProofVerification{},
			},
			target: ErrAssetProofPathUnknownPassiveAssets,
		},
		{
			name: "nil inventory result",
			verifier: &testConfirmedProofVerifier{
				result: nil,
			},
			target: ErrAssetProofPathUnknownPassiveAssets,
		},
		{
			name: "known passive asset",
			verifier: &testConfirmedProofVerifier{
				result: &ConfirmedProofVerification{
					AnchorAssetInventoryComplete: true,
					PassiveAssetCount:            1,
				},
			},
			target: ErrAssetProofPathPassiveAssets,
		},
		{
			name: "base verification failure",
			verifier: &testConfirmedProofVerifier{
				err: errors.New("backend unavailable"),
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := path.Verify(
				context.Background(), testCase.verifier,
			)
			require.Error(t, err)
			if testCase.target != nil {
				require.ErrorIs(t, err, testCase.target)
			}
		})
	}

	tamperedWitness := mutateAssetProofPathTransition(
		t, fixture.transitionProof, func(p *proof.Proof) {
			p.Asset.PrevWitnesses[0].TxWitness[0][0] ^= 1
		},
	)
	tamperedPath := path.Clone()
	tamperedPath.Steps[0].TransitionProof = tamperedWitness
	_, err := tamperedPath.Verify(
		context.Background(), &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
	)
	require.ErrorContains(t, err, "verify unconfirmed step 0")

	// A repeated step no longer chains off the first step's tip, so the
	// second step's witness binding fails.
	brokenChain := path.Clone()
	brokenChain.Steps = append(brokenChain.Steps, brokenChain.Steps[0])
	_, err = brokenChain.Verify(
		context.Background(), &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
	)
	require.ErrorContains(t, err, "verify step 1 input binding")
}

// TestAssetProofPathEncodingRejectsCorruption checks that decode is bounded,
// checksummed, and leaves the receiver untouched on every error.
func TestAssetProofPathEncodingRejectsCorruption(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: fixture.transitionProof,
		}},
	}
	encoded, err := path.MarshalBinary()
	require.NoError(t, err)

	badChecksum := cloneBytes(encoded)
	badChecksum[len(badChecksum)-1] ^= 1
	unknownFormat := cloneBytes(encoded)
	binary.BigEndian.PutUint16(
		unknownFormat[len(assetProofPathMagic):], 99,
	)
	recomputeAssetProofPathChecksum(unknownFormat)
	badMagic := cloneBytes(encoded)
	badMagic[0] ^= 1
	recomputeAssetProofPathChecksum(badMagic)
	trailingByte := append(cloneBytes(encoded), 0)
	recomputeAssetProofPathChecksum(trailingByte)

	testCases := []struct {
		name    string
		encoded []byte
		target  error
	}{
		{
			name:    "truncated",
			encoded: encoded[:assetProofPathHeaderSize],
			target:  ErrAssetProofPathInvalid,
		},
		{
			name:    "checksum",
			encoded: badChecksum,
			target:  ErrAssetProofPathInvalid,
		},
		{
			name:    "format",
			encoded: unknownFormat,
			target:  ErrAssetProofPathUnknownFormat,
		},
		{
			name:    "magic",
			encoded: badMagic,
			target:  ErrAssetProofPathInvalid,
		},
		{
			name:    "trailing byte",
			encoded: trailingByte,
			target:  ErrAssetProofPathInvalid,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			receiver := AssetProofPath{
				ConfirmedBaseProof: []byte("unchanged"),
			}
			err := receiver.UnmarshalBinary(testCase.encoded)
			require.ErrorIs(t, err, testCase.target)
			require.Equal(
				t, []byte("unchanged"), receiver.ConfirmedBaseProof,
			)
			require.Nil(t, receiver.Steps)
		})
	}

	tooDeep := path.Clone()
	tooDeep.Steps = make(
		[]AssetProofPathStep, AssetProofPathMaxDepth+1,
	)
	for i := range tooDeep.Steps {
		tooDeep.Steps[i].TransitionProof = fixture.transitionProof
	}
	require.ErrorContains(t, tooDeep.Validate(), "path depth")

	oversized := path.Clone()
	oversized.Steps[0].TransitionProof = make(
		[]byte, AssetProofPathMaxStepSize+1,
	)
	require.ErrorContains(t, oversized.Validate(), "transition proof exceeds")
}

type assetProofPathFixture struct {
	baseProofFile   []byte
	baseProof       *proof.Proof
	transitionProof []byte
	transition      *proof.Proof
}

func newAssetProofPathFixture(t *testing.T) *assetProofPathFixture {
	t.Helper()

	baseProofFile, baseProof, senderKey := newAssetProofPathBase(t)
	transition := newAssetProofPathTransition(
		t, baseProof, senderKey, testPrivateKey(t, 3),
	)
	transitionProof, err := transition.Bytes()
	require.NoError(t, err)

	previous := &proof.AssetSnapshot{
		Asset:    baseProof.Asset.Copy(),
		OutPoint: baseProof.OutPoint(),
	}
	_, err = transition.Verify(
		context.Background(), previous, assetProofPathChainLookup{},
		proof.VerifierCtx{},
		proof.WithSkipChainVerification(),
	)
	require.NoError(t, err)

	return &assetProofPathFixture{
		baseProofFile:   baseProofFile,
		baseProof:       baseProof,
		transitionProof: transitionProof,
		transition:      transition,
	}
}

func newAssetProofPathBase(t *testing.T) ([]byte, *proof.Proof,
	*btcec.PrivateKey) {

	return newAssetProofPathBaseWithGroup(t, false)
}

func newGroupedAssetProofPathBase(t *testing.T) ([]byte, *proof.Proof,
	*btcec.PrivateKey) {

	return newAssetProofPathBaseWithGroup(t, true)
}

func newAssetProofPathBaseWithGroup(t *testing.T, grouped bool) (
	[]byte, *proof.Proof, *btcec.PrivateKey) {

	return newAssetProofPathBaseVariant(t, grouped, 2)
}

// newAssetProofPathSecondBase mints the same asset under a different
// anchor internal key, so the two bases share an identity but sit at
// distinct outpoints — the shape two boarded outputs of one asset have.
func newAssetProofPathSecondBase(t *testing.T) ([]byte, *proof.Proof,
	*btcec.PrivateKey) {

	return newAssetProofPathBaseVariant(t, false, 7)
}

func newAssetProofPathBaseVariant(t *testing.T, grouped bool,
	internalKeyIndex byte) ([]byte, *proof.Proof, *btcec.PrivateKey) {

	t.Helper()

	senderKey := testPrivateKey(t, 1)
	internalKey := testPrivateKey(t, internalKeyIndex)
	var genesisHash chainhash.Hash
	genesisHash[0] = 1
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash:  genesisHash,
			Index: 1,
		},
		Tag:         "asset-proof-path",
		OutputIndex: 0,
		Type:        asset.Normal,
	}
	amount := uint64(100)
	var groupKey *asset.GroupKey
	if grouped {
		protoAsset, err := asset.New(
			genesis, amount, 0, 0,
			asset.NewScriptKeyBip86(keychain.KeyDescriptor{
				PubKey: senderKey.PubKey(),
			}), nil, asset.WithAssetVersion(asset.V1),
		)
		require.NoError(t, err)
		groupKey, _ = asset.RandGroupKeyWithSigner(
			t, testPrivateKey(t, 9), genesis, protoAsset,
		)
	}
	commitmentVersion := commitment.TapCommitmentV2
	tapCommitment, assets, err := commitment.Mint(
		&commitmentVersion, genesis, groupKey, &commitment.AssetDetails{
			Version: asset.V1,
			Type:    asset.Normal,
			ScriptKey: keychain.KeyDescriptor{
				PubKey: senderKey.PubKey(),
			},
			Amount: &amount,
		},
	)
	require.NoError(t, err)
	require.Len(t, assets, 1)

	anchorTx := assetProofPathAnchorTx(
		t, genesis.FirstPrevOut, internalKey.PubKey(), tapCommitment,
	)
	block := assetProofPathBlock(anchorTx)
	proofs, err := proof.NewMintingBlobs(
		&proof.MintParams{
			Block:            block,
			BlockHeight:      100,
			Tx:               anchorTx,
			TxIndex:          0,
			OutputIndex:      0,
			InternalKey:      internalKey.PubKey(),
			TaprootAssetRoot: tapCommitment,
			GenesisPoint:     genesis.FirstPrevOut,
		}, proof.MockVerifierCtx,
		proof.WithGenOption(proof.WithVersion(proof.TransitionV1)),
	)
	require.NoError(t, err)

	key := asset.ToSerialized(assets[0].ScriptKey.PubKey)
	baseProof, ok := proofs[key]
	require.True(t, ok)
	baseProofFile, err := proof.EncodeAsProofFile(baseProof)
	require.NoError(t, err)
	var proofFile proof.File
	require.NoError(t, proofFile.Decode(bytes.NewReader(baseProofFile)))
	_, err = proofFile.Verify(context.Background(), proof.MockVerifierCtx)
	require.NoError(t, err)

	return baseProofFile, baseProof, senderKey
}

func newAssetProofPathTransition(t *testing.T, previous *proof.Proof,
	spendKey, recipientKey *btcec.PrivateKey) *proof.Proof {

	return newAssetProofPathTransitionWithHiddenAsset(
		t, previous, spendKey, recipientKey, false,
	)
}

func newAssetProofPathTransitionWithHiddenAsset(t *testing.T,
	previous *proof.Proof, spendKey, recipientKey *btcec.PrivateKey,
	hiddenAsset bool) *proof.Proof {

	return newAssetProofPathTransitionWithMutator(
		t, previous, spendKey, recipientKey, hiddenAsset, nil,
	)
}

func newAssetProofPathTransitionWithMutator(t *testing.T,
	previous *proof.Proof, spendKey, recipientKey *btcec.PrivateKey,
	hiddenAsset bool, mutate func(*asset.Asset)) *proof.Proof {

	t.Helper()

	newAsset := previous.Asset.Copy()
	newAsset.ScriptKey = asset.NewScriptKeyBip86(keychain.KeyDescriptor{
		PubKey: recipientKey.PubKey(),
	})
	if mutate != nil {
		mutate(newAsset)
	}
	previousID := &asset.PrevID{
		OutPoint: previous.OutPoint(),
		ID:       previous.Asset.ID(),
		ScriptKey: asset.ToSerialized(
			previous.Asset.ScriptKey.PubKey,
		),
	}
	newAsset.PrevWitnesses = []asset.Witness{{
		PrevID: previousID,
	}}

	inputs := commitment.InputSet{
		*previousID: &previous.Asset,
	}
	virtualTx, _, err := tapscript.VirtualTx(newAsset, inputs)
	require.NoError(t, err)
	virtualTx = asset.VirtualTxWithInput(
		virtualTx, newAsset.LockTime, newAsset.RelativeLockTime, 0, nil,
	)
	sigHash, err := tapscript.InputKeySpendSigHash(
		virtualTx, &previous.Asset, newAsset, 0,
		txscript.SigHashDefault,
	)
	require.NoError(t, err)
	signingKey := spendKey
	tweakedSpendKey := txscript.TweakTaprootPrivKey(*spendKey, nil)
	if bytes.Equal(
		schnorr.SerializePubKey(tweakedSpendKey.PubKey()),
		schnorr.SerializePubKey(previous.Asset.ScriptKey.PubKey),
	) {

		signingKey = tweakedSpendKey
	} else {
		require.Equal(t,
			schnorr.SerializePubKey(spendKey.PubKey()),
			schnorr.SerializePubKey(previous.Asset.ScriptKey.PubKey),
		)
	}
	signature, err := schnorr.Sign(signingKey, sigHash)
	require.NoError(t, err)
	newAsset.PrevWitnesses[0].TxWitness = wire.TxWitness{
		signature.Serialize(),
	}

	assetCommitment, err := commitment.NewAssetCommitment(newAsset)
	require.NoError(t, err)
	commitmentVersion := commitment.TapCommitmentV2
	tapCommitment, err := commitment.NewTapCommitment(
		&commitmentVersion, assetCommitment,
	)
	require.NoError(t, err)
	spentAsset, err := asset.MakeSpentAsset(newAsset.PrevWitnesses[0])
	require.NoError(t, err)
	require.NoError(t, tapCommitment.MergeAltLeaves(
		asset.ToAltLeaves([]*asset.Asset{spentAsset}),
	))
	spenders, err := asset.CollectSpenders(newAsset)
	require.NoError(t, err)
	require.NoError(t, tapCommitment.MergeAltLeaves(spenders))
	if hiddenAsset {
		hiddenGenesis := asset.Genesis{
			FirstPrevOut: previous.OutPoint(),
			Tag:          "hidden-co-anchored-asset",
			OutputIndex:  0,
			Type:         asset.Normal,
		}
		hiddenAmount := uint64(1)
		hiddenTapCommitment, _, err := commitment.Mint(
			&commitmentVersion, hiddenGenesis, nil,
			&commitment.AssetDetails{
				Version: asset.V1,
				Type:    asset.Normal,
				ScriptKey: keychain.KeyDescriptor{
					PubKey: testPrivateKey(t, 8).PubKey(),
				},
				Amount: &hiddenAmount,
			},
		)
		require.NoError(t, err)
		require.NoError(t, tapCommitment.Merge(hiddenTapCommitment))
	}

	anchorTx := assetProofPathAnchorTx(
		t, previous.OutPoint(), recipientKey.PubKey(), tapCommitment,
	)
	transition, err := proof.CreateTransitionProof(
		previous.OutPoint(), &proof.TransitionParams{
			Block:            assetProofPathBlock(anchorTx),
			Tx:               anchorTx,
			TxIndex:          0,
			OutputIndex:      0,
			InternalKey:      recipientKey.PubKey(),
			TaprootAssetRoot: tapCommitment,
			NewAsset:         newAsset,
		}, proof.WithVersion(proof.TransitionV1),
	)
	require.NoError(t, err)

	return transition
}

func newAssetProofPathSplitTransition(t *testing.T, previous *proof.Proof,
	spendKey *btcec.PrivateKey) *proof.Proof {

	t.Helper()

	rootKey := testPrivateKey(t, 4)
	selectedKey := testPrivateKey(t, 5)
	rootLocator := &commitment.SplitLocator{
		OutputIndex:  0,
		AssetID:      previous.Asset.ID(),
		ScriptKey:    asset.ToSerialized(rootKey.PubKey()),
		Amount:       60,
		AssetVersion: asset.V1,
	}
	selectedLocator := &commitment.SplitLocator{
		OutputIndex:  1,
		AssetID:      previous.Asset.ID(),
		ScriptKey:    asset.ToSerialized(selectedKey.PubKey()),
		Amount:       40,
		AssetVersion: asset.V1,
	}
	splitCommitment, err := commitment.NewSplitCommitment(
		context.Background(), []commitment.SplitCommitmentInput{{
			Asset:    &previous.Asset,
			OutPoint: previous.OutPoint(),
		}}, rootLocator, selectedLocator,
	)
	require.NoError(t, err)

	rootAsset := splitCommitment.RootAsset
	rootSplit := splitCommitment.SplitAssets[*rootLocator]
	rootLocatorProof := &rootSplit.PrevWitnesses[0].SplitCommitment.Proof
	selectedAsset := &splitCommitment.SplitAssets[*selectedLocator].Asset
	signAssetProofPathTransition(
		t, previous, rootAsset, spendKey, []*asset.Asset{selectedAsset},
	)

	selectedAssetNoProof := selectedAsset.Copy()
	selectedAssetNoProof.PrevWitnesses[0].SplitCommitment = nil
	rootAssetCommitment, err := commitment.NewAssetCommitment(rootAsset)
	require.NoError(t, err)
	selectedAssetCommitment, err := commitment.NewAssetCommitment(
		selectedAssetNoProof,
	)
	require.NoError(t, err)
	commitmentVersion := commitment.TapCommitmentV2
	rootTapCommitment, err := commitment.NewTapCommitment(
		&commitmentVersion, rootAssetCommitment,
	)
	require.NoError(t, err)
	selectedTapCommitment, err := commitment.NewTapCommitment(
		&commitmentVersion, selectedAssetCommitment,
	)
	require.NoError(t, err)

	spentAsset, err := asset.MakeSpentAsset(rootAsset.PrevWitnesses[0])
	require.NoError(t, err)
	require.NoError(t, rootTapCommitment.MergeAltLeaves(
		asset.ToAltLeaves([]*asset.Asset{spentAsset}),
	))
	spenders, err := asset.CollectSpenders(rootAsset)
	require.NoError(t, err)
	require.NoError(t, rootTapCommitment.MergeAltLeaves(spenders))

	rootInternalKey := testPrivateKey(t, 6).PubKey()
	selectedInternalKey := testPrivateKey(t, 7).PubKey()
	anchorTx := &wire.MsgTx{
		Version: 3,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: previous.OutPoint(),
		}},
		TxOut: []*wire.TxOut{
			assetProofPathAnchorOutput(
				t, rootInternalKey, rootTapCommitment,
			),
			assetProofPathAnchorOutput(
				t, selectedInternalKey, selectedTapCommitment,
			),
		},
	}

	_, selectedInRootProof, err := rootTapCommitment.Proof(
		selectedAsset.TapCommitmentKey(),
		selectedAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)

	transition, err := proof.CreateTransitionProof(
		previous.OutPoint(), &proof.TransitionParams{
			Block:            assetProofPathBlock(anchorTx),
			Tx:               anchorTx,
			TxIndex:          0,
			OutputIndex:      1,
			InternalKey:      selectedInternalKey,
			TaprootAssetRoot: selectedTapCommitment,
			ExclusionProofs: []proof.TaprootProof{{
				OutputIndex: 0,
				InternalKey: rootInternalKey,
				CommitmentProof: &proof.CommitmentProof{
					Proof: *selectedInRootProof,
				},
			}},
			NewAsset:             selectedAsset,
			RootOutputIndex:      0,
			RootInternalKey:      rootInternalKey,
			RootTaprootAssetTree: rootTapCommitment,
			RootLocatorProof:     rootLocatorProof,
		}, proof.WithVersion(proof.TransitionV1),
	)
	require.NoError(t, err)

	return transition
}

func signAssetProofPathTransition(t *testing.T, previous *proof.Proof,
	rootAsset *asset.Asset, spendKey *btcec.PrivateKey,
	splitAssets []*asset.Asset) {

	t.Helper()

	inputs := commitment.InputSet{}
	for _, witness := range rootAsset.PrevWitnesses {
		require.NotNil(t, witness.PrevID)
		inputs[*witness.PrevID] = &previous.Asset
	}
	virtualTx, _, err := tapscript.VirtualTx(rootAsset, inputs)
	require.NoError(t, err)
	virtualTx = asset.VirtualTxWithInput(
		virtualTx, rootAsset.LockTime, rootAsset.RelativeLockTime, 0,
		nil,
	)
	sigHash, err := tapscript.InputKeySpendSigHash(
		virtualTx, &previous.Asset, rootAsset, 0,
		txscript.SigHashDefault,
	)
	require.NoError(t, err)
	tweakedSpendKey := txscript.TweakTaprootPrivKey(*spendKey, nil)
	signature, err := schnorr.Sign(tweakedSpendKey, sigHash)
	require.NoError(t, err)
	witness := wire.TxWitness{signature.Serialize()}
	rootAsset.PrevWitnesses[0].TxWitness = witness

	for _, splitAsset := range splitAssets {
		splitWitness := splitAsset.PrevWitnesses[0].SplitCommitment
		require.NotNil(t, splitWitness)
		splitWitness.RootAsset.PrevWitnesses[0].TxWitness = witness
	}
}

func assetProofPathAnchorTx(t *testing.T, previous wire.OutPoint,
	internalKey *btcec.PublicKey,
	tapCommitment *commitment.TapCommitment) *wire.MsgTx {

	t.Helper()

	return &wire.MsgTx{
		Version: 3,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: previous,
			Sequence:         0,
		}},
		TxOut: []*wire.TxOut{
			assetProofPathAnchorOutput(t, internalKey, tapCommitment),
		},
	}
}

func assetProofPathAnchorOutput(t *testing.T,
	internalKey *btcec.PublicKey,
	tapCommitment *commitment.TapCommitment) *wire.TxOut {

	t.Helper()

	tapscriptRoot := tapCommitment.TapscriptRoot(nil)
	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, tapscriptRoot[:],
	)
	outputScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return &wire.TxOut{
		Value:    330,
		PkScript: outputScript,
	}
}

func assetProofPathBlock(anchorTx *wire.MsgTx) *wire.MsgBlock {
	tree := blockchain.BuildMerkleTreeStore(
		[]*btcutil.Tx{btcutil.NewTx(anchorTx)}, false,
	)
	merkleRoot := tree[len(tree)-1]

	return &wire.MsgBlock{
		Header: wire.BlockHeader{
			MerkleRoot: *merkleRoot,
		},
		Transactions: []*wire.MsgTx{anchorTx},
	}
}

func testPrivateKey(t *testing.T, value byte) *btcec.PrivateKey {
	t.Helper()

	keyBytes := make([]byte, 32)
	keyBytes[len(keyBytes)-1] = value
	privateKey, _ := btcec.PrivKeyFromBytes(keyBytes)
	return privateKey
}

func mutateAssetProofPathTransition(t *testing.T, rawProof []byte,
	mutate func(*proof.Proof)) []byte {

	t.Helper()

	transition, err := proof.Decode(rawProof)
	require.NoError(t, err)
	mutate(transition)
	mutated, err := transition.Bytes()
	require.NoError(t, err)
	return mutated
}

func recomputeAssetProofPathChecksum(encoded []byte) {
	bodyEnd := len(encoded) - assetProofPathChecksumSize
	checksum := sha256.Sum256(encoded[:bodyEnd])
	copy(encoded[bodyEnd:], checksum[:])
}

type testConfirmedProofVerifier struct {
	result                 *ConfirmedProofVerification
	err                    error
	calls                  int
	unconfirmedErr         error
	unconfirmedCalls       int
	unconfirmedTransitions []UnconfirmedAnchorVerification
}

func (v *testConfirmedProofVerifier) VerifyConfirmedProof(
	ctx context.Context,
	proofFileBytes []byte) (*ConfirmedProofVerification, error) {

	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	if v.result == nil {
		return nil, nil
	}

	var proofFile proof.File
	if err := proofFile.Decode(bytes.NewReader(proofFileBytes)); err != nil {
		return nil, err
	}
	if _, err := proofFile.Verify(
		ctx, proof.MockVerifierCtx,
	); err != nil {
		return nil, err
	}

	result := *v.result
	return &result, nil
}

func (v *testConfirmedProofVerifier) VerifyUnconfirmedAnchor(
	_ context.Context, transition UnconfirmedAnchorVerification) error {

	v.unconfirmedCalls++
	transition.AnchorTransaction = bytes.Clone(transition.AnchorTransaction)
	v.unconfirmedTransitions = append(v.unconfirmedTransitions, transition)
	return v.unconfirmedErr
}

type confirmedOnlyProofVerifier struct{}

func (confirmedOnlyProofVerifier) VerifyConfirmedProof(
	context.Context, []byte) (*ConfirmedProofVerification, error) {

	return &ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
	}, nil
}
