package tapsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/stretchr/testify/require"
)

// TestAssetProofPathCoInputRoundTrip proves a recursive co-input path survives
// the binary codec and deep-clones its co-input tree.
func TestAssetProofPathCoInputRoundTrip(t *testing.T) {
	t.Parallel()

	merge := newAssetProofPathMergeFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: merge.firstBaseFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: merge.transitionProof,
			CoInputPaths: []*AssetProofPath{{
				ConfirmedBaseProof: merge.secondBaseFile,
			}},
		}},
	}

	require.NoError(t, path.Validate())

	encoded, err := path.MarshalBinary()
	require.NoError(t, err)

	var decoded AssetProofPath
	require.NoError(t, decoded.UnmarshalBinary(encoded))
	require.Equal(t, path, &decoded)

	// Re-encoding the decoded path must reproduce the exact bytes.
	reEncoded, err := decoded.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, encoded, reEncoded)

	// A clone must not share co-input path bytes with the original.
	clone := decoded.Clone()
	require.Equal(t, &decoded, clone)
	clone.Steps[0].CoInputPaths[0].ConfirmedBaseProof[0] ^= 1
	require.NotEqual(
		t, clone.Steps[0].CoInputPaths[0].ConfirmedBaseProof,
		decoded.Steps[0].CoInputPaths[0].ConfirmedBaseProof,
	)
}

// TestAssetProofPathMidPathMerge proves a second step can merge a co-input
// path that carries its own unconfirmed transition: both spines chain from
// their confirmed bases and the merged tip carries the combined amount.
func TestAssetProofPathMidPathMerge(t *testing.T) {
	t.Parallel()

	firstFile, firstProof, firstKey := newAssetProofPathBase(t)
	secondFile, secondProof, secondKey := newAssetProofPathSecondBase(t)

	// Spine: A -> A'. Co-input path: B -> B'. Merge: A' + B' -> C.
	spineKey := testPrivateKey(t, 13)
	spineTransition := newAssetProofPathTransition(
		t, firstProof, firstKey, spineKey,
	)
	spineBytes, err := spineTransition.Bytes()
	require.NoError(t, err)

	coKey := testPrivateKey(t, 14)
	coTransition := newAssetProofPathTransition(
		t, secondProof, secondKey, coKey,
	)
	coBytes, err := coTransition.Bytes()
	require.NoError(t, err)

	mergeTransition := newAssetProofPathMergeTransition(
		t, []*proof.Proof{spineTransition, coTransition},
		[]*btcec.PrivateKey{spineKey, coKey}, testPrivateKey(t, 15),
	)
	mergeBytes, err := mergeTransition.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: firstFile,
		Steps: []AssetProofPathStep{
			{TransitionProof: spineBytes},
			{
				TransitionProof: mergeBytes,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: secondFile,
					Steps: []AssetProofPathStep{{
						TransitionProof: coBytes,
					}},
				}},
			},
		},
	}
	require.NoError(t, path.Validate())

	encoded, err := path.MarshalBinary()
	require.NoError(t, err)
	var decoded AssetProofPath
	require.NoError(t, decoded.UnmarshalBinary(encoded))
	require.Equal(t, path, &decoded)

	verifier := &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	}
	summary, err := decoded.Verify(context.Background(), verifier)
	require.NoError(t, err)
	require.Equal(t, uint16(2), summary.Depth)
	require.Equal(t, uint64(200), summary.Amount)
	require.Equal(
		t, outpointFromWire(mergeTransition.OutPoint()),
		summary.AnchorOutpoint,
	)

	// Both confirmed bases are host-verified, and every unconfirmed
	// anchor is attested: spine A', co-input B', then the merge.
	require.Equal(t, 2, verifier.calls)
	require.Equal(t, 3, verifier.unconfirmedCalls)
	mergeAttestation := verifier.unconfirmedTransitions[2]
	require.Equal(t, uint16(1), mergeAttestation.StepIndex)
	require.Equal(t, []Outpoint{
		outpointFromWire(spineTransition.OutPoint()),
		outpointFromWire(coTransition.OutPoint()),
	}, mergeAttestation.PreviousAnchorOutpoints)
	coAttestation := verifier.unconfirmedTransitions[1]
	require.Equal(t, uint16(0), coAttestation.StepIndex)
	require.Equal(
		t, outpointFromWire(secondProof.OutPoint()),
		coAttestation.PreviousAnchorOutpoint,
	)
}

// TestAssetProofPathMergeAndSplit proves the Ark transition shape: one
// step spends two inputs and splits them into a recipient leaf and a change
// leaf, with the recipient leaf selected by the path.
func TestAssetProofPathMergeAndSplit(t *testing.T) {
	t.Parallel()

	firstFile, firstProof, firstKey := newAssetProofPathBase(t)
	secondFile, secondProof, secondKey := newAssetProofPathSecondBase(t)

	transition := newAssetProofPathMergeSplitTransition(
		t, []*proof.Proof{firstProof, secondProof},
		[]*btcec.PrivateKey{firstKey, secondKey}, 140,
	)
	transitionBytes, err := transition.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: firstFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transitionBytes,
			CoInputPaths: []*AssetProofPath{{
				ConfirmedBaseProof: secondFile,
			}},
		}},
	}
	require.NoError(t, path.Validate())

	stepSummary, err := path.Steps[0].Summary()
	require.NoError(t, err)
	require.True(t, stepSummary.SplitAsset)
	require.Equal(t, uint64(140), stepSummary.Amount)

	verifier := &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	}
	summary, err := path.Verify(context.Background(), verifier)
	require.NoError(t, err)
	require.Equal(t, uint16(1), summary.Depth)
	require.Equal(t, uint64(140), summary.Amount)
	require.Equal(
		t, outpointFromWire(transition.OutPoint()),
		summary.AnchorOutpoint,
	)
	require.Equal(t, 2, verifier.calls)
	require.Equal(t, 1, verifier.unconfirmedCalls)
	require.Equal(t, []Outpoint{
		outpointFromWire(firstProof.OutPoint()),
		outpointFromWire(secondProof.OutPoint()),
	}, verifier.unconfirmedTransitions[0].PreviousAnchorOutpoints)
}

// TestAssetProofPathNestedCoPath proves a co-input path that itself merges
// another co-input path verifies end to end at nesting depth two.
func TestAssetProofPathNestedCoPath(t *testing.T) {
	t.Parallel()

	firstFile, firstProof, firstKey := newAssetProofPathBase(t)
	secondFile, secondProof, secondKey := newAssetProofPathSecondBase(t)
	thirdFile, thirdProof, thirdKey := newAssetProofPathBaseVariant(
		t, false, 12,
	)

	// Inner merge: B + C -> D. Outer merge: A + D -> E.
	innerKey := testPrivateKey(t, 16)
	innerMerge := newAssetProofPathMergeTransition(
		t, []*proof.Proof{secondProof, thirdProof},
		[]*btcec.PrivateKey{secondKey, thirdKey}, innerKey,
	)
	innerBytes, err := innerMerge.Bytes()
	require.NoError(t, err)

	outerMerge := newAssetProofPathMergeTransition(
		t, []*proof.Proof{firstProof, innerMerge},
		[]*btcec.PrivateKey{firstKey, innerKey}, testPrivateKey(t, 17),
	)
	outerBytes, err := outerMerge.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: firstFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: outerBytes,
			CoInputPaths: []*AssetProofPath{{
				ConfirmedBaseProof: secondFile,
				Steps: []AssetProofPathStep{{
					TransitionProof: innerBytes,
					CoInputPaths: []*AssetProofPath{{
						ConfirmedBaseProof: thirdFile,
					}},
				}},
			}},
		}},
	}
	require.NoError(t, path.Validate())

	encoded, err := path.MarshalBinary()
	require.NoError(t, err)
	var decoded AssetProofPath
	require.NoError(t, decoded.UnmarshalBinary(encoded))
	require.Equal(t, path, &decoded)

	verifier := &testConfirmedProofVerifier{
		result: &ConfirmedProofVerification{
			AnchorAssetInventoryComplete: true,
		},
	}
	summary, err := decoded.Verify(context.Background(), verifier)
	require.NoError(t, err)
	require.Equal(t, uint64(300), summary.Amount)
	require.Equal(
		t, outpointFromWire(outerMerge.OutPoint()),
		summary.AnchorOutpoint,
	)

	// All three confirmed bases are host-verified, and both merges are
	// attested with their complete input sets.
	require.Equal(t, 3, verifier.calls)
	require.Equal(t, 2, verifier.unconfirmedCalls)
	require.Equal(t, []Outpoint{
		outpointFromWire(secondProof.OutPoint()),
		outpointFromWire(thirdProof.OutPoint()),
	}, verifier.unconfirmedTransitions[0].PreviousAnchorOutpoints)
	require.Equal(t, []Outpoint{
		outpointFromWire(firstProof.OutPoint()),
		outpointFromWire(innerMerge.OutPoint()),
	}, verifier.unconfirmedTransitions[1].PreviousAnchorOutpoints)
}

// TestAssetProofPathVerifyFailsClosed covers the money-critical rejection
// paths of merging steps: unbound co-inputs, anchors that do not consume
// them, and identity drift between the spine and a co-input tip.
func TestAssetProofPathVerifyFailsClosed(t *testing.T) {
	t.Parallel()

	firstFile, firstProof, firstKey := newAssetProofPathBase(t)
	secondFile, secondProof, secondKey := newAssetProofPathSecondBase(t)
	verifier := func() *testConfirmedProofVerifier {
		return &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		}
	}
	mergeTransition := newAssetProofPathMergeTransition(
		t, []*proof.Proof{firstProof, secondProof},
		[]*btcec.PrivateKey{firstKey, secondKey}, testPrivateKey(t, 3),
	)
	mergeBytes, err := mergeTransition.Bytes()
	require.NoError(t, err)

	t.Run("duplicate co-input outpoint", func(t *testing.T) {
		t.Parallel()

		// The co-path resolves to the spine's own tip, so the two
		// declared inputs collapse to one outpoint.
		path := &AssetProofPath{
			ConfirmedBaseProof: firstFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: mergeBytes,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: firstFile,
				}},
			}},
		}
		_, err := path.Verify(context.Background(), verifier())
		require.ErrorIs(t, err, ErrAssetProofPathInvalid)
		require.ErrorContains(t, err, "duplicate co-input outpoint")
	})

	t.Run("anchor does not spend co-input", func(t *testing.T) {
		t.Parallel()

		// The witnesses still claim both inputs, but the Bitcoin
		// transaction only consumes the spine tip. The native
		// verifier does not check co-input prevouts, so the local
		// anchor-spend check must.
		mutated := mutateAssetProofPathTransition(
			t, mergeBytes, func(p *proof.Proof) {
				p.AnchorTx.TxIn = p.AnchorTx.TxIn[:1]
			},
		)
		path := &AssetProofPath{
			ConfirmedBaseProof: firstFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: mutated,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: secondFile,
				}},
			}},
		}
		_, err := path.Verify(context.Background(), verifier())
		require.ErrorIs(t, err, ErrAssetProofPathInvalid)
		require.ErrorContains(t, err, "anchor inputs")
		require.ErrorContains(t, err, "exactly once, found 0")
	})

	t.Run("undeclared witness outpoint", func(t *testing.T) {
		t.Parallel()

		// A merging transition whose second witness spends an
		// outpoint no co-path accounts for is rejected before the
		// native verifier runs.
		unrelatedFile, _, _ := newAssetProofPathBaseVariant(
			t, false, 12,
		)
		path := &AssetProofPath{
			ConfirmedBaseProof: firstFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: mergeBytes,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: unrelatedFile,
				}},
			}},
		}
		_, err := path.Verify(context.Background(), verifier())
		require.ErrorIs(t, err, ErrAssetProofPathInvalid)
		require.ErrorContains(t, err, "undeclared outpoint")
	})

	t.Run("co-input identity mismatch", func(t *testing.T) {
		t.Parallel()

		groupedFile, groupedProof, groupedKey :=
			newGroupedAssetProofPathBase(t)
		grouped := newAssetProofPathMergeTransition(
			t, []*proof.Proof{firstProof, groupedProof},
			[]*btcec.PrivateKey{firstKey, groupedKey},
			testPrivateKey(t, 3),
		)
		groupedBytes, err := grouped.Bytes()
		require.NoError(t, err)

		path := &AssetProofPath{
			ConfirmedBaseProof: firstFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: groupedBytes,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: groupedFile,
				}},
			}},
		}
		_, err = path.Verify(context.Background(), verifier())
		require.ErrorIs(t, err, ErrAssetProofPathInvalid)
		require.ErrorContains(
			t, err, "co-input asset group key changed",
		)
	})
}

// newAssetProofPathMergeSplitTransition spends any number of predecessors of
// the same asset into a recipient split and a change split, returning the
// proof of the selected recipient leaf.
func newAssetProofPathMergeSplitTransition(t *testing.T,
	sources []*proof.Proof, keys []*btcec.PrivateKey,
	recipientAmount uint64) *proof.Proof {

	t.Helper()
	require.Len(t, keys, len(sources))

	total := uint64(0)
	for _, source := range sources {
		total += source.Asset.Amount
	}
	require.Less(t, recipientAmount, total)

	changeKey := testPrivateKey(t, 21)
	recipientKey := testPrivateKey(t, 22)
	changeLocator := &commitment.SplitLocator{
		OutputIndex:  0,
		AssetID:      sources[0].Asset.ID(),
		ScriptKey:    asset.ToSerialized(changeKey.PubKey()),
		Amount:       total - recipientAmount,
		AssetVersion: asset.V1,
	}
	recipientLocator := &commitment.SplitLocator{
		OutputIndex:  1,
		AssetID:      sources[0].Asset.ID(),
		ScriptKey:    asset.ToSerialized(recipientKey.PubKey()),
		Amount:       recipientAmount,
		AssetVersion: asset.V1,
	}
	splitInputs := make([]commitment.SplitCommitmentInput, len(sources))
	sourceIndex := make(map[wire.OutPoint]int, len(sources))
	for i, source := range sources {
		splitInputs[i] = commitment.SplitCommitmentInput{
			Asset:    &source.Asset,
			OutPoint: source.OutPoint(),
		}
		sourceIndex[source.OutPoint()] = i
	}
	splitCommitment, err := commitment.NewSplitCommitment(
		context.Background(), splitInputs, changeLocator,
		recipientLocator,
	)
	require.NoError(t, err)

	rootAsset := splitCommitment.RootAsset
	rootSplit := splitCommitment.SplitAssets[*changeLocator]
	rootLocatorProof := &rootSplit.PrevWitnesses[0].SplitCommitment.Proof
	selectedAsset := &splitCommitment.SplitAssets[*recipientLocator].Asset

	// Sign every root witness with the key of the source it spends, then
	// mirror the complete witness set into the selected leaf's embedded
	// root asset.
	inputs := commitment.InputSet{}
	for _, witness := range rootAsset.PrevWitnesses {
		require.NotNil(t, witness.PrevID)
		index, ok := sourceIndex[witness.PrevID.OutPoint]
		require.True(t, ok)
		inputs[*witness.PrevID] = &sources[index].Asset
	}
	virtualTx, _, err := tapscript.VirtualTx(rootAsset, inputs)
	require.NoError(t, err)
	for i := range rootAsset.PrevWitnesses {
		witness := &rootAsset.PrevWitnesses[i]
		index := sourceIndex[witness.PrevID.OutPoint]
		inputTx := asset.VirtualTxWithInput(
			virtualTx, rootAsset.LockTime,
			rootAsset.RelativeLockTime, uint32(i), nil,
		)
		sigHash, err := tapscript.InputKeySpendSigHash(
			inputTx, &sources[index].Asset, rootAsset, uint32(i),
			txscript.SigHashDefault,
		)
		require.NoError(t, err)
		signature, err := schnorr.Sign(
			txscript.TweakTaprootPrivKey(*keys[index], nil),
			sigHash,
		)
		require.NoError(t, err)
		witness.TxWitness = wire.TxWitness{signature.Serialize()}
	}
	embeddedRoot := &selectedAsset.PrevWitnesses[0].SplitCommitment.
		RootAsset
	for i := range rootAsset.PrevWitnesses {
		embeddedRoot.PrevWitnesses[i].TxWitness =
			rootAsset.PrevWitnesses[i].TxWitness
	}

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

	spentAssets := make([]*asset.Asset, 0, len(rootAsset.PrevWitnesses))
	for i := range rootAsset.PrevWitnesses {
		spent, err := asset.MakeSpentAsset(rootAsset.PrevWitnesses[i])
		require.NoError(t, err)
		spentAssets = append(spentAssets, spent)
	}
	require.NoError(t, rootTapCommitment.MergeAltLeaves(
		asset.ToAltLeaves(spentAssets),
	))
	spenders, err := asset.CollectSpenders(rootAsset)
	require.NoError(t, err)
	require.NoError(t, rootTapCommitment.MergeAltLeaves(spenders))

	changeInternalKey := testPrivateKey(t, 23).PubKey()
	recipientInternalKey := testPrivateKey(t, 24).PubKey()
	anchorTx := &wire.MsgTx{
		Version: 3,
		TxOut: []*wire.TxOut{
			assetProofPathAnchorOutput(
				t, changeInternalKey, rootTapCommitment,
			),
			assetProofPathAnchorOutput(
				t, recipientInternalKey,
				selectedTapCommitment,
			),
		},
	}
	for _, source := range sources {
		anchorTx.TxIn = append(anchorTx.TxIn, &wire.TxIn{
			PreviousOutPoint: source.OutPoint(),
		})
	}

	_, selectedInRootProof, err := rootTapCommitment.Proof(
		selectedAsset.TapCommitmentKey(),
		selectedAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)

	transition, err := proof.CreateTransitionProof(
		sources[0].OutPoint(), &proof.TransitionParams{
			Block:            assetProofPathBlock(anchorTx),
			Tx:               anchorTx,
			TxIndex:          0,
			OutputIndex:      1,
			InternalKey:      recipientInternalKey,
			TaprootAssetRoot: selectedTapCommitment,
			ExclusionProofs: []proof.TaprootProof{{
				OutputIndex: 0,
				InternalKey: changeInternalKey,
				CommitmentProof: &proof.CommitmentProof{
					Proof: *selectedInRootProof,
				},
			}},
			NewAsset:             selectedAsset,
			RootOutputIndex:      0,
			RootInternalKey:      changeInternalKey,
			RootTaprootAssetTree: rootTapCommitment,
			RootLocatorProof:     rootLocatorProof,
		}, proof.WithVersion(proof.TransitionV1),
	)
	require.NoError(t, err)

	return transition
}

// TestAssetProofPathValidateRejections checks recursive co-input bounds.
func TestAssetProofPathValidateRejections(t *testing.T) {
	t.Parallel()

	merge := newAssetProofPathMergeFixture(t)
	coPath := func() *AssetProofPath {
		return &AssetProofPath{
			ConfirmedBaseProof: merge.secondBaseFile,
		}
	}
	mergePath := func() *AssetProofPath {
		return &AssetProofPath{
			ConfirmedBaseProof: merge.firstBaseFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: merge.transitionProof,
				CoInputPaths: []*AssetProofPath{
					coPath(),
				},
			}},
		}
	}

	t.Run("co-input path count over bound", func(t *testing.T) {
		t.Parallel()

		path := mergePath()
		for range AssetProofPathMaxStepCoPaths {
			path.Steps[0].CoInputPaths = append(
				path.Steps[0].CoInputPaths, coPath(),
			)
		}
		require.ErrorContains(t, path.Validate(), "co-input paths")
		require.ErrorContains(t, path.Validate(), "limit is 15")
	})

	t.Run("co-input path depth over bound", func(t *testing.T) {
		t.Parallel()

		// Nest one level beyond the depth bound. The inner levels use
		// garbage transition bytes: the shape pass must reject the
		// depth before decoding any content.
		path := coPath()
		for range AssetProofPathMaxCoPathDepth + 1 {
			path = &AssetProofPath{
				ConfirmedBaseProof: merge.firstBaseFile,
				Steps: []AssetProofPathStep{{
					TransitionProof: []byte{1},
					CoInputPaths: []*AssetProofPath{
						path,
					},
				}},
			}
		}
		require.ErrorContains(
			t, path.Validate(), "co-input path depth exceeds 3",
		)
	})

	t.Run("witness count pinned to co-input paths", func(t *testing.T) {
		t.Parallel()

		single := newAssetProofPathFixture(t)
		path := &AssetProofPath{
			ConfirmedBaseProof: single.baseProofFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: single.transitionProof,
				CoInputPaths: []*AssetProofPath{
					coPath(),
				},
			}},
		}
		require.ErrorContains(
			t, path.Validate(),
			"must contain 2 complete asset witnesses, found 1",
		)
	})

	t.Run("whole-tree size budget", func(t *testing.T) {
		t.Parallel()

		// Three co-input paths of 22 MiB stay under every per-blob
		// bound but blow the shared 64 MiB tree budget. The bases are
		// garbage: the budget must trip before content is decoded.
		hugeBase := make([]byte, 22*1024*1024)
		path := mergePath()
		path.Steps[0].CoInputPaths = nil
		for range 3 {
			path.Steps[0].CoInputPaths = append(
				path.Steps[0].CoInputPaths, &AssetProofPath{
					ConfirmedBaseProof: hugeBase,
				},
			)
		}
		require.ErrorContains(
			t, path.Validate(), "encoded path exceeds",
		)
	})
}

// TestAssetProofPathRejectsHostileEncodings proves decode-time bounds trip
// before nested content is parsed and unknown future formats are rejected.
func TestAssetProofPathRejectsHostileEncodings(t *testing.T) {
	t.Parallel()

	merge := newAssetProofPathMergeFixture(t)

	t.Run("deep nesting fails fast", func(t *testing.T) {
		t.Parallel()

		// Fifty nested levels of garbage payloads: decoding must stop
		// at the depth bound without parsing the deeper levels.
		blob := encodeTestAssetProofPathBlob(
			t, merge.secondBaseFile, nil, nil,
		)
		for range 50 {
			blob = encodeTestAssetProofPathBlob(
				t, merge.firstBaseFile, []byte{1},
				[][]byte{blob},
			)
		}

		var decoded AssetProofPath
		err := decoded.UnmarshalBinary(blob)
		require.ErrorIs(t, err, ErrAssetProofPathInvalid)
		require.ErrorContains(t, err, "co-input path depth exceeds 3")
		require.Nil(t, decoded.ConfirmedBaseProof)
	})

	t.Run("co-input count checked before blobs", func(t *testing.T) {
		t.Parallel()

		// The declared count exceeds the bound and the garbage blobs
		// after it could never parse: the count check must trip first.
		garbage := make([][]byte, AssetProofPathMaxStepCoPaths+1)
		for i := range garbage {
			garbage[i] = []byte{0xff}
		}
		blob := encodeTestAssetProofPathBlob(
			t, merge.firstBaseFile, []byte{1}, garbage,
		)

		var decoded AssetProofPath
		err := decoded.UnmarshalBinary(blob)
		require.ErrorIs(t, err, ErrAssetProofPathInvalid)
		require.ErrorContains(t, err, "limit is 15")
	})

	t.Run("unknown format rejected", func(t *testing.T) {
		t.Parallel()

		path := &AssetProofPath{
			ConfirmedBaseProof: merge.firstBaseFile,
			Steps: []AssetProofPathStep{{
				TransitionProof: merge.transitionProof,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: merge.
						secondBaseFile,
				}},
			}},
		}
		encoded, err := path.MarshalBinary()
		require.NoError(t, err)
		binary.BigEndian.PutUint16(
			encoded[len(assetProofPathMagic):],
			assetProofPathFormat+1,
		)
		recomputeAssetProofPathChecksum(encoded)

		var decoded AssetProofPath
		err = decoded.UnmarshalBinary(encoded)
		require.ErrorIs(t, err, ErrAssetProofPathUnknownFormat)
	})
}

// encodeTestAssetProofPathBlob writes the raw wire layout directly so hostile
// shapes that MarshalBinary refuses to produce can be exercised. A nil
// transitionProof encodes a stepless path.
func encodeTestAssetProofPathBlob(t *testing.T, base,
	transitionProof []byte, coBlobs [][]byte) []byte {

	t.Helper()

	var body bytes.Buffer
	body.Write(assetProofPathMagic[:])
	require.NoError(
		t, binary.Write(&body, binary.BigEndian, assetProofPathFormat),
	)
	require.NoError(t, writeAssetProofPathBytes(&body, base))

	stepCount := uint16(0)
	if transitionProof != nil {
		stepCount = 1
	}
	require.NoError(
		t, binary.Write(&body, binary.BigEndian, stepCount),
	)
	if transitionProof != nil {
		require.NoError(
			t, writeAssetProofPathBytes(&body, transitionProof),
		)
		require.NoError(t, binary.Write(
			&body, binary.BigEndian, uint16(len(coBlobs)),
		))
		for _, blob := range coBlobs {
			require.NoError(
				t, writeAssetProofPathBytes(&body, blob),
			)
		}
	}

	checksum := sha256.Sum256(body.Bytes())

	return append(body.Bytes(), checksum[:]...)
}
