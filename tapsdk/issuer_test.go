package tapsdk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const testFungibleAssetName = "token"

func TestIssuerCreateFungibleMintsGroupedAsset(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)
	feeRate := mustFeeRateSatPerVByte(t, 1)

	groupRef := AssetRefFromGroupKey(testKey(t, 2))
	record := issuerAssetRecord(
		issuerAssetID(1), groupRef, AssetTypeFungible,
		testFungibleAssetName, 100,
	)
	batchKey := testKey(t, 3)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Once()
	client.On("MintAsset", ctx, mock.MatchedBy(
		func(req *MintAssetRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.Asset != nil &&
				req.Asset.AssetType == AssetTypeFungible &&
				req.Asset.Name == testFungibleAssetName &&
				req.Asset.InitialSupply == 100 &&
				req.Asset.AllowIssuance
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FinalizeBatch", ctx, mock.MatchedBy(
		func(req *FinalizeBatchRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.FeeRate == feeRate
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{record}, nil,
	).Once()

	asset, err := issuer.CreateFungible(ctx, FungibleAssetSpec{
		Name:   testFungibleAssetName,
		Amount: 100,
	}, WithMintFeeRate(feeRate))
	require.NoError(t, err)
	require.Equal(t, groupRef, asset.AssetRef)
	require.Equal(t, AssetTypeFungible, asset.Type)
	require.Equal(t, uint64(100), asset.Amount)

	client.AssertExpectations(t)
}

func TestIssuerCreateFungibleSignsExternalIssuance(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)
	feeRate := mustFeeRateSatPerVByte(t, 1)

	externalKey := ExternalKey{
		XPub:           "tpub-external",
		DerivationPath: "m/86'/1'/0'/0/0",
		MasterFingerprint: [4]byte{
			0xde, 0xad, 0xbe, 0xef,
		},
	}
	groupKey := testKey(t, 2)
	groupRef := AssetRefFromGroupKey(groupKey)
	scriptKey := &ScriptKey{PubKey: testKey(t, 30)}
	issuanceID := issuerAssetID(31)
	record := issuerAssetRecord(
		issuanceID, groupRef, AssetTypeFungible, testFungibleAssetName,
		100,
	)
	batchKey := testKey(t, 3)
	unsignedPsbt := "unsigned-issuance-psbt"
	signedPsbt := "signed-issuance-psbt"

	signer := &recordingIssuanceSigner{
		signedPsbt: signedPsbt,
	}

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Once()
	client.On("MintAsset", ctx, mock.MatchedBy(
		func(req *MintAssetRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.Asset != nil &&
				req.Asset.AssetType == AssetTypeFungible &&
				req.Asset.Name == testFungibleAssetName &&
				req.Asset.InitialSupply == 100 &&
				req.Asset.AllowIssuance &&
				req.Asset.ExternalGroupKey != nil &&
				req.Asset.ExternalGroupKey.XPub == externalKey.XPub
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FundBatch", ctx, mock.MatchedBy(
		func(req *FundBatchRequest) bool {
			return req != nil && req.FeeRate == feeRate
		},
	)).Return(&VerboseMintingBatch{
		Batch: MintingBatch{BatchKey: batchKey},
		UnsealedAssets: []UnsealedMintAsset{
			{
				Asset: &PendingMintAsset{
					AssetType: AssetTypeFungible,
					Name:      testFungibleAssetName,
					Amount:    100,
					ScriptKey: scriptKey,
				},
				GroupKeyRequest: &GroupKeyRequest{
					AnchorGenesis: &GenesisInfo{
						GenesisPoint: "genesis:0",
						Name:         testFungibleAssetName,
						IssuanceID:   issuanceID,
						AssetType:    AssetTypeFungible,
					},
					ExternalKey: &externalKey,
				},
				GroupVirtualTx: &GroupVirtualTx{
					GenesisID:  issuanceID,
					TweakedKey: &groupKey,
				},
				GroupVirtualPSBT: unsignedPsbt,
			},
		},
	}, nil).Once()
	client.On("SealBatch", ctx, mock.MatchedBy(
		func(req *SealBatchRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				len(req.SignedGroupVirtualPSBTs) == 1 &&
				req.SignedGroupVirtualPSBTs[0] == signedPsbt
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FinalizeBatch", ctx, mock.MatchedBy(
		func(req *FinalizeBatchRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.FeeRate.IsZero()
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{record}, nil,
	).Once()

	asset, err := issuer.CreateFungible(
		ctx, FungibleAssetSpec{
			Name:      testFungibleAssetName,
			Amount:    100,
			ScriptKey: scriptKey,
		},
		WithMintFeeRate(feeRate),
		WithExternalIssuanceKey(externalKey),
		WithExternalIssuanceSigner(signer),
	)
	require.NoError(t, err)
	require.Equal(t, groupRef, asset.AssetRef)
	require.Len(t, signer.requests, 1)

	req := signer.requests[0]
	require.Equal(t, IssuanceOperationCreateAsset, req.Operation)
	require.Equal(t, groupRef, req.AssetRef)
	require.Equal(t, testFungibleAssetName, req.Name)
	require.Equal(t, uint64(100), req.Amount)
	require.Equal(t, unsignedPsbt, req.VirtualPSBT)
	require.Equal(t, externalKey, req.ExternalKey)
	require.Equal(t, scriptKey, req.ScriptKey)
	require.NotNil(t, req.AnchorGenesis)
	require.Equal(t, issuanceID, req.AnchorGenesis.IssuanceID)

	client.AssertExpectations(t)
}

func TestIssuerIssueFungibleRejectsNFTCollection(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	collectionRef := AssetRefFromGroupKey(testKey(t, 4))
	record := issuerAssetRecord(
		issuerAssetID(2), collectionRef, AssetTypeNFT,
		"item", 1,
	)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&collectionRef),
	).Return([]*AssetRecord{record}, nil).Once()

	_, err := issuer.IssueFungible(ctx, collectionRef, 50)
	require.ErrorIs(t, err, ErrWrongAssetType)

	client.AssertExpectations(t)
}

type recordingIssuanceSigner struct {
	signedPsbt string
	requests   []IssuanceSigningRequest
}

func (s *recordingIssuanceSigner) SignIssuance(_ context.Context,
	req IssuanceSigningRequest) (SignedIssuance, error) {

	s.requests = append(s.requests, req)

	return SignedIssuance{VirtualPSBT: s.signedPsbt}, nil
}

func TestIssuerIssueFungibleMintsIssuance(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	groupRef := AssetRefFromGroupKey(testKey(t, 8))
	first := issuerAssetRecord(
		issuerAssetID(8), groupRef, AssetTypeFungible,
		testFungibleAssetName, 100,
	)
	second := issuerAssetRecord(
		issuerAssetID(9), groupRef, AssetTypeFungible,
		testFungibleAssetName, 50,
	)
	batchKey := testKey(t, 9)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&groupRef),
	).Return([]*AssetRecord{first}, nil).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&groupRef),
	).Return([]*AssetRecord{first}, nil).Once()
	client.On("MintIssuance", ctx, mock.MatchedBy(
		func(req *MintIssuanceRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.Issuance != nil &&
				req.Issuance.AssetRef == groupRef &&
				req.Issuance.AssetType == AssetTypeFungible &&
				req.Issuance.Name == testFungibleAssetName &&
				req.Issuance.Amount == 50
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FinalizeBatch", ctx, mock.Anything).Return(
		&MintingBatch{BatchKey: batchKey}, nil,
	).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&groupRef),
	).Return([]*AssetRecord{first, second}, nil).Once()

	issuance, err := issuer.IssueFungible(ctx, groupRef, 50)
	require.NoError(t, err)
	require.Equal(t, groupRef, issuance.AssetRef)
	require.Equal(t, second.Genesis.IssuanceID, issuance.IssuanceID)
	require.Equal(t, uint64(50), issuance.Amount)

	client.AssertExpectations(t)
}

func TestIssuerCreateNFTMintsAsset(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	itemRef := AssetRefFromAssetID(issuerAssetID(10))
	record := issuerAssetRecord(
		issuerAssetID(10), itemRef, AssetTypeNFT,
		"nft", 1,
	)
	batchKey := testKey(t, 10)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Once()
	client.On("MintAsset", ctx, mock.MatchedBy(
		func(req *MintAssetRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.Asset != nil &&
				req.Asset.AssetType == AssetTypeNFT &&
				req.Asset.Name == "nft" &&
				req.Asset.InitialSupply == 1 &&
				!req.Asset.AllowIssuance
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FinalizeBatch", ctx, mock.Anything).Return(
		&MintingBatch{BatchKey: batchKey}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{record}, nil,
	).Once()

	asset, err := issuer.CreateNFT(ctx, NFTSpec{Name: "nft"})
	require.NoError(t, err)
	require.Equal(t, itemRef, asset.AssetRef)
	require.Equal(t, AssetTypeNFT, asset.Type)

	client.AssertExpectations(t)
}

func TestIssuerCreateCollectionReturnsResult(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	collectionRef := AssetRefFromGroupKey(testKey(t, 11))
	record := issuerAssetRecord(
		issuerAssetID(11), collectionRef, AssetTypeNFT,
		testFirst, 1,
	)
	batchKey := testKey(t, 12)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Once()
	client.On("MintAsset", ctx, mock.MatchedBy(
		func(req *MintAssetRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.Asset != nil &&
				req.Asset.AssetType == AssetTypeNFT &&
				req.Asset.Name == testFirst &&
				req.Asset.AllowIssuance
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FinalizeBatch", ctx, mock.Anything).Return(
		&MintingBatch{BatchKey: batchKey}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{record}, nil,
	).Once()

	result, err := issuer.CreateCollection(
		ctx, NFTSpec{Name: testFirst},
	)
	require.NoError(t, err)
	require.Equal(t, collectionRef, result.Collection.AssetRef)
	require.Equal(t, uint64(1), result.Collection.ItemCount)
	require.True(t, result.FirstItem.AssetRef.IsAssetIDRef())
	require.NotNil(t, result.FirstItem.CollectionRef)
	require.Equal(t, collectionRef, *result.FirstItem.CollectionRef)

	client.AssertExpectations(t)
}

func TestIssuerMintCollectionItemMintsCollectibleIssuance(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	collectionRef := AssetRefFromGroupKey(testKey(t, 5))
	first := issuerAssetRecord(
		issuerAssetID(3), collectionRef, AssetTypeNFT,
		testFirst, 1,
	)
	second := issuerAssetRecord(
		issuerAssetID(4), collectionRef, AssetTypeNFT,
		testSecond, 1,
	)
	batchKey := testKey(t, 6)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&collectionRef),
	).Return([]*AssetRecord{first}, nil).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&collectionRef),
	).Return([]*AssetRecord{first}, nil).Once()
	client.On("MintIssuance", ctx, mock.MatchedBy(
		func(req *MintIssuanceRequest) bool {
			return req != nil &&
				req.ShortResponse &&
				req.Issuance != nil &&
				req.Issuance.AssetRef == collectionRef &&
				req.Issuance.AssetType == AssetTypeNFT &&
				req.Issuance.Name == testSecond &&
				req.Issuance.Amount == 1
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("FinalizeBatch", ctx, mock.MatchedBy(
		func(req *FinalizeBatchRequest) bool {
			return req != nil && req.ShortResponse
		},
	)).Return(&MintingBatch{BatchKey: batchKey}, nil).Once()
	client.On("ListAssetRecords", ctx,
		includeUnconfirmed(&collectionRef),
	).Return([]*AssetRecord{first, second}, nil).Once()

	asset, err := issuer.MintCollectionItem(ctx, collectionRef,
		NFTSpec{Name: testSecond},
	)
	require.NoError(t, err)
	require.True(t, asset.AssetRef.IsAssetIDRef())
	require.NotNil(t, asset.CollectionRef)
	require.Equal(t, collectionRef, *asset.CollectionRef)
	require.Equal(t, testSecond, asset.Name)

	client.AssertExpectations(t)
}

func TestIssuerRejectsActiveBatch(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{
			{
				Batch: MintingBatch{
					BatchKey: testKey(t, 7),
					State:    BatchStatePending,
				},
			},
		}, nil,
	).Once()

	_, err := issuer.CreateNFT(ctx, NFTSpec{Name: "blocked"})
	require.ErrorIs(t, err, ErrMintBatchActive)

	client.AssertExpectations(t)
}

func TestIssuerResolveTimeoutIsDistinct(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)

	batchKey := testKey(t, 13)

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Once()
	client.On("MintAsset", ctx, mock.Anything).Return(
		&MintingBatch{BatchKey: batchKey}, nil,
	).Once()
	client.On("FinalizeBatch", ctx, mock.Anything).Return(
		&MintingBatch{BatchKey: batchKey}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Maybe()

	_, err := issuer.CreateFungible(
		ctx, FungibleAssetSpec{Name: "slow", Amount: 1},
		WithMintResolveTimeout(time.Nanosecond),
	)
	require.ErrorIs(t, err, ErrMintResolveTimeout)
	require.NotErrorIs(t, err, ErrMintResultNotFound)

	client.AssertExpectations(t)
}

func TestIssuerFinalizeFailureCancelsBatch(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	issuer := NewIssuer(client)
	finalizeErr := errors.New("finalize failed")

	client.On("ListBatches", ctx, mock.Anything).Return(
		[]*VerboseMintingBatch{}, nil,
	).Once()
	client.On("ListAssetRecords", ctx, includeUnconfirmed(nil)).Return(
		[]*AssetRecord{}, nil,
	).Once()
	client.On("MintAsset", ctx, mock.Anything).Return(
		&MintingBatch{BatchKey: testKey(t, 14)}, nil,
	).Once()
	client.On("FinalizeBatch", ctx, mock.Anything).Return(
		nil, finalizeErr,
	).Once()
	client.On("CancelBatch", mock.Anything).Return(
		&CancelBatchResponse{}, nil,
	).Once()

	_, err := issuer.CreateNFT(ctx, NFTSpec{Name: "cancel"})
	require.ErrorIs(t, err, finalizeErr)

	client.AssertExpectations(t)
}

func includeUnconfirmed(
	ref *AssetRef) any {

	return mock.MatchedBy(func(req *ListAssetsRequest) bool {
		if req == nil || !req.IncludeUnconfirmedMints {
			return false
		}
		if ref == nil {
			return req.AssetRef == nil
		}
		if req.AssetRef == nil {
			return false
		}

		return req.AssetRef.Equivalent(*ref)
	})
}

func issuerAssetRecord(id AssetID, ref AssetRef,
	assetType AssetType, name string,
	amount uint64) *AssetRecord {

	return &AssetRecord{
		AssetRef: ref,
		Genesis: IssuanceGenesis{
			Tag:        name,
			IssuanceID: id,
			Type:       assetType,
		},
		Amount: amount,
	}
}

func issuerAssetID(seed byte) AssetID {
	var id AssetID
	for i := range id {
		id[i] = seed + byte(i)
	}

	return id
}
