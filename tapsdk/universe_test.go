package tapsdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUniverseHasAssetAndGetRoots(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(1))
	root := testUniverseRoot(ref, ProofTypeIssuance)

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(&QueryRootResponse{
		IssuanceRoot: root,
	}, nil).Twice()

	unknown := AssetRefFromAssetID(universeAssetID(2))
	mc.On("QueryAssetRoots", ctx,
		universeID(unknown, ProofTypeIssuance),
	).Return(&QueryRootResponse{}, nil).Twice()

	universe := NewUniverse(mc)
	ok, err := universe.HasAsset(ctx, ref)
	require.NoError(t, err)
	require.True(t, ok)

	roots, err := universe.GetRoots(ctx, ref)
	require.NoError(t, err)
	require.True(t, roots.HasRoots())
	require.Same(t, root, roots.IssuanceRoot)

	ok, err = universe.HasAsset(ctx, unknown)
	require.NoError(t, err)
	require.False(t, ok)

	_, err = universe.GetRoots(ctx, unknown)
	require.ErrorIs(t, err, ErrAssetUnknown)

	mc.AssertExpectations(t)
}

func TestUniverseHasAssetPropagatesRPCError(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(3))

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(nil, status.Error(codes.Unauthenticated, "missing macaroon")).
		Once()

	ok, err := NewUniverse(mc).HasAsset(ctx, ref)
	require.ErrorIs(t, err, ErrUnauthenticated)
	require.False(t, ok)

	mc.AssertExpectations(t)
}

func TestUniverseGetRootsEmptyRootIsUnknown(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(4))

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(&QueryRootResponse{
		IssuanceRoot: &UniverseRoot{},
	}, nil).Once()

	_, err := NewUniverse(mc).GetRoots(ctx, ref)
	require.ErrorIs(t, err, ErrAssetUnknown)

	mc.AssertExpectations(t)
}

func TestUniverseListAndGetProofs(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(5))
	issuanceRoot := testUniverseRoot(
		ref, ProofTypeIssuance,
	)
	transferRoot := testUniverseRoot(
		ref, ProofTypeTransfer,
	)
	roots := &QueryRootResponse{
		IssuanceRoot: issuanceRoot,
		TransferRoot: transferRoot,
	}
	issuanceKey := testLeafKey(t, 1)
	transferKey := testLeafKey(t, 2)

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(roots, nil).Twice()
	mc.On("AssetLeafKeys", ctx, &AssetLeafKeysRequest{
		ID: *universeID(ref, ProofTypeIssuance),
	}).Return([]AssetLeafKey{issuanceKey}, nil).Once()
	mc.On("AssetLeafKeys", ctx, &AssetLeafKeysRequest{
		ID: *universeID(ref, ProofTypeTransfer),
	}).Return([]AssetLeafKey{transferKey}, nil).Once()
	mc.On("QueryProof", ctx, &UniverseKey{
		ID:      *universeID(ref, ProofTypeIssuance),
		LeafKey: issuanceKey,
	}).Return(testProofResponse(issuanceKey, issuanceRoot, []byte{1}), nil).Once()
	mc.On("QueryProof", ctx, &UniverseKey{
		ID:      *universeID(ref, ProofTypeTransfer),
		LeafKey: transferKey,
	}).Return(testProofResponse(transferKey, transferRoot, []byte{2}), nil).
		Twice()

	universe := NewUniverse(mc)
	proofs, err := universe.ListProofs(ctx, ref)
	require.NoError(t, err)
	require.Len(t, proofs, 2)
	require.Equal(t, ProofTypeIssuance, proofs[0].ProofType)
	require.Equal(t, []byte{1}, proofs[0].Proof)
	require.Equal(t, ProofTypeTransfer, proofs[1].ProofType)
	require.Equal(t, []byte{2}, proofs[1].Proof)

	proof, err := universe.GetProof(
		ctx, ref, transferKey,
		WithUniverseProofType(ProofTypeTransfer),
	)
	require.NoError(t, err)
	require.Equal(t, ProofTypeTransfer, proof.ProofType)
	require.Equal(t, transferKey, proof.LeafKey)

	mc.AssertExpectations(t)
}

func TestUniverseListProofsKnownAssetNoProofs(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(6))

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(&QueryRootResponse{
		IssuanceRoot: testUniverseRoot(
			ref, ProofTypeIssuance,
		),
	}, nil).Once()
	mc.On("AssetLeafKeys", ctx, &AssetLeafKeysRequest{
		ID: *universeID(ref, ProofTypeIssuance),
	}).Return([]AssetLeafKey{}, nil).Once()

	_, err := NewUniverse(mc).ListProofs(
		ctx, ref,
		WithUniverseProofType(ProofTypeIssuance),
	)
	require.ErrorIs(t, err, ErrNoProofs)

	mc.AssertExpectations(t)
}

func TestUniverseListProofsOptions(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(7))
	root := testUniverseRoot(ref, ProofTypeIssuance)
	key := testLeafKey(t, 7)

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(&QueryRootResponse{
		IssuanceRoot: root,
	}, nil).Once()
	mc.On("AssetLeafKeys", ctx, &AssetLeafKeysRequest{
		ID:        *universeID(ref, ProofTypeIssuance),
		Offset:    2,
		Limit:     3,
		Direction: SortAscending,
	}).Return([]AssetLeafKey{key}, nil).Once()
	mc.On("QueryProof", ctx, &UniverseKey{
		ID:      *universeID(ref, ProofTypeIssuance),
		LeafKey: key,
	}).Return(testProofResponse(key, root, []byte{3}), nil).Once()

	proofs, err := NewUniverse(mc).ListProofs(
		ctx, ref,
		WithUniverseProofType(ProofTypeIssuance),
		WithUniverseProofPage(2, 3),
		WithUniverseProofDirection(SortAscending),
	)
	require.NoError(t, err)
	require.Len(t, proofs, 1)
	require.Equal(t, ProofTypeIssuance, proofs[0].ProofType)

	mc.AssertExpectations(t)
}

func TestUniverseGetProofFallbackToTransfer(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(8))
	issuanceRoot := testUniverseRoot(
		ref, ProofTypeIssuance,
	)
	transferRoot := testUniverseRoot(
		ref, ProofTypeTransfer,
	)
	key := testLeafKey(t, 8)

	mc := new(mockClient)
	mc.On("QueryAssetRoots", ctx,
		universeID(ref, ProofTypeIssuance),
	).Return(&QueryRootResponse{
		IssuanceRoot: issuanceRoot,
		TransferRoot: transferRoot,
	}, nil).Once()
	mc.On("QueryProof", ctx, &UniverseKey{
		ID:      *universeID(ref, ProofTypeIssuance),
		LeafKey: key,
	}).Return(nil, status.Error(codes.NotFound, "proof missing")).Once()
	mc.On("QueryProof", ctx, &UniverseKey{
		ID:      *universeID(ref, ProofTypeTransfer),
		LeafKey: key,
	}).Return(testProofResponse(key, transferRoot, []byte{4}), nil).Once()

	proof, err := NewUniverse(mc).GetProof(ctx, ref, key)
	require.NoError(t, err)
	require.Equal(t, ProofTypeTransfer, proof.ProofType)
	require.Equal(t, []byte{4}, proof.Proof)

	mc.AssertExpectations(t)
}

func TestUniverseProofOptionValidation(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(9))
	universe := NewUniverse(new(mockClient))

	_, err := universe.ListProofs(ctx, ref, WithUniverseProofPage(1, 2))
	require.ErrorIs(t, err, ErrUniverseProofTypeRequired)

	_, err = universe.ListProofs(
		ctx, ref,
		WithUniverseProofType(ProofTypeIssuance),
		WithUniverseProofPage(-1, 2),
	)
	require.ErrorIs(t, err, ErrInvalidPagination)

	_, err = universe.GetProof(
		ctx, ref, AssetLeafKey{},
		WithUniverseProofType(ProofTypeUnspecified),
	)
	require.ErrorIs(t, err, ErrUniverseProofTypeRequired)
}

func TestUniverseSyncAsset(t *testing.T) {
	ctx := context.Background()
	ref := AssetRefFromAssetID(universeAssetID(10))
	root := testUniverseRoot(ref, ProofTypeIssuance)

	mc := new(mockClient)
	mc.On("SyncUniverse", ctx, mock.MatchedBy(
		func(req *SyncRequest) bool {
			return req.UniverseHost == "tapd-alice:10029" &&
				req.SyncMode == SyncFull &&
				len(req.SyncTargets) == 2 &&
				req.SyncTargets[0].ID == *universeID(
					ref, ProofTypeIssuance,
				) &&
				req.SyncTargets[1].ID == *universeID(
					ref, ProofTypeTransfer,
				)
		},
	)).Return([]SyncedUniverse{{
		NewAssetRoot: root,
	}}, nil).Once()

	result, err := NewUniverse(mc).SyncAsset(ctx, ref, "tapd-alice:10029")
	require.NoError(t, err)
	require.Equal(t, ref, result.AssetRef)
	require.NotNil(t, result.Issuance)
	require.Nil(t, result.Transfer)

	_, err = NewUniverse(mc).SyncAsset(ctx, ref, "")
	require.ErrorIs(t, err, ErrUniverseHostRequired)

	mc.AssertExpectations(t)
}

func TestUniverseSyncAssets(t *testing.T) {
	ctx := context.Background()
	ref1 := AssetRefFromAssetID(universeAssetID(11))
	ref2 := AssetRefFromAssetID(universeAssetID(12))
	root1 := testUniverseRoot(ref1, ProofTypeIssuance)
	root2 := testUniverseRoot(ref2, ProofTypeIssuance)

	mc := new(mockClient)
	mc.On("SyncUniverse", ctx, mock.MatchedBy(
		func(req *SyncRequest) bool {
			return req.UniverseHost == "tapd-alice:10029" &&
				req.SyncMode == SyncIssuanceOnly &&
				len(req.SyncTargets) == 2 &&
				req.SyncTargets[0].ID == *universeID(
					ref1, ProofTypeIssuance,
				) &&
				req.SyncTargets[1].ID == *universeID(
					ref2, ProofTypeIssuance,
				)
		},
	)).Return([]SyncedUniverse{{
		NewAssetRoot: root2,
	}, {
		NewAssetRoot: root1,
	}}, nil).Once()

	results, err := NewUniverse(mc).SyncAssets(
		ctx, []AssetRef{ref1, ref2}, "tapd-alice:10029",
		WithUniverseSyncMode(SyncIssuanceOnly),
	)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, ref1, results[0].AssetRef)
	require.Equal(t, ref2, results[1].AssetRef)
	require.Equal(t, root1.ID, results[0].Issuance.NewAssetRoot.ID)
	require.Equal(t, root2.ID, results[1].Issuance.NewAssetRoot.ID)

	mc.AssertExpectations(t)
}

func TestUniverseValidation(t *testing.T) {
	ctx := context.Background()
	universe := NewUniverse(new(mockClient))

	_, err := universe.GetRoots(ctx, "")
	require.ErrorIs(t, err, ErrNoAssetRef)

	_, err = universe.GetRoots(ctx, AssetRef("not-a-ref"))
	require.ErrorIs(t, err, ErrInvalidAssetRef)

	_, err = universe.GetProof(
		ctx, AssetRef("not-a-ref"), AssetLeafKey{},
	)
	require.ErrorIs(t, err, ErrInvalidAssetRef)

	_, err = universe.SyncAssets(ctx, []AssetRef{
		AssetRef("not-a-ref"),
	}, "tapd-alice:10029")
	require.ErrorIs(t, err, ErrInvalidAssetRef)
	require.NotErrorIs(t, err, ErrUniverseHostRequired)

	_, err = universe.SyncAssets(ctx, []AssetRef{
		AssetRef("not-a-ref"),
	}, "")
	require.ErrorIs(t, err, ErrInvalidAssetRef)
	require.NotErrorIs(t, err, ErrUniverseHostRequired)

	_, err = universe.SyncAssets(ctx, nil, "tapd-alice:10029")
	require.ErrorIs(t, err, ErrNoAssetRef)

	ref := AssetRefFromAssetID(universeAssetID(13))
	_, err = universe.SyncAssets(
		ctx, []AssetRef{ref, ref}, "tapd-alice:10029",
	)
	require.ErrorIs(t, err, ErrDuplicateAssetRef)

	_, err = universe.SyncAsset(ctx, ref, "https://tapd.example:10029")
	require.ErrorIs(t, err, ErrInvalidUniverseHost)
}

func universeAssetID(seed byte) AssetID {
	var id AssetID
	for i := range id {
		id[i] = seed + byte(i)
	}

	return id
}

func testUniverseRoot(ref AssetRef,
	proofType ProofType) *UniverseRoot {

	return &UniverseRoot{
		ID: *universeID(ref, proofType),
		MSSMTRoot: &MerkleSumNode{
			RootSum: 1,
		},
		AssetName: "asset",
	}
}

func testLeafKey(t *testing.T, seed byte) AssetLeafKey {
	t.Helper()

	return AssetLeafKey{
		Outpoint: Outpoint{
			Txid:  [32]byte(universeAssetID(seed)),
			Index: uint32(seed),
		},
		ScriptKey: testRefGroupKey(t),
	}
}

func testProofResponse(leafKey AssetLeafKey,
	root *UniverseRoot, proof []byte) *AssetProofResponse {

	return &AssetProofResponse{
		Key: UniverseKey{
			ID:      root.ID,
			LeafKey: leafKey,
		},
		UniverseRoot: root,
		AssetLeaf: &AssetLeaf{
			Proof: proof,
		},
	}
}
