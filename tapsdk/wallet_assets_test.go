package tapsdk

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListAssets_AggregatesGroupedFungibleIssuances(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	groupRef := AssetRefFromGroupKey(testKey(t, 41))
	firstID := bundleAssetID(11)
	secondID := bundleAssetID(12)
	req := &ListAssetsRequest{AssetRef: &groupRef}

	mc.On("ListAssetRecords", ctx, req).Return([]*AssetRecord{
		bundleAsset(groupRef, firstID, testKey(t, 42), 100,
			AssetTypeNormal),
		bundleAsset(groupRef, secondID, testKey(t, 43), 250,
			AssetTypeNormal),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Len(t, assets, 1)

	asset := assets[0]
	require.Equal(t, groupRef, asset.AssetRef)
	require.Equal(t, AssetTypeNormal, asset.Type)
	require.Equal(t, uint64(350), asset.Amount)
	require.Nil(t, asset.CollectionRef)

	mc.AssertExpectations(t)
}

func TestListAssets_SingleCollectible(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	assetID := bundleAssetID(13)
	assetRef := AssetRefFromAssetID(assetID)
	req := &ListAssetsRequest{AssetRef: &assetRef}

	mc.On("ListAssetRecords", ctx, req).Return([]*AssetRecord{
		bundleAsset(assetRef, assetID, testKey(t, 44), 1,
			AssetTypeCollectible),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Len(t, assets, 1)

	asset := assets[0]
	require.Equal(t, assetRef, asset.AssetRef)
	require.Equal(t, AssetTypeCollectible, asset.Type)
	require.Equal(t, uint64(1), asset.Amount)
	require.Nil(t, asset.CollectionRef)

	mc.AssertExpectations(t)
}

func TestListAssets_ReturnsCollectionItemsAsNFTAssets(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	collectionRef := AssetRefFromGroupKey(testKey(t, 45))
	firstID := bundleAssetID(14)
	secondID := bundleAssetID(15)
	req := &ListAssetsRequest{AssetRef: &collectionRef}

	mc.On("ListAssetRecords", ctx, req).Return([]*AssetRecord{
		bundleAsset(collectionRef, firstID, testKey(t, 46), 1,
			AssetTypeCollectible),
		bundleAsset(collectionRef, secondID, testKey(t, 47), 1,
			AssetTypeCollectible),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Len(t, assets, 2)

	require.Equal(t, AssetRefFromAssetID(firstID), assets[0].AssetRef)
	require.Equal(t, AssetTypeCollectible, assets[0].Type)
	require.Equal(t, uint64(1), assets[0].Amount)
	require.NotNil(t, assets[0].CollectionRef)
	require.Equal(t, collectionRef, *assets[0].CollectionRef)
	require.Equal(t, AssetRefFromAssetID(secondID), assets[1].AssetRef)
	require.NotNil(t, assets[1].CollectionRef)
	require.Equal(t, collectionRef, *assets[1].CollectionRef)

	mc.AssertExpectations(t)
}

func TestListCollections_AggregatesCollectionItems(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	collectionRef := AssetRefFromGroupKey(testKey(t, 54))
	req := &ListCollectionsRequest{AssetRef: &collectionRef}

	mc.On("ListAssetRecords", ctx, &ListAssetsRequest{
		AssetRef: &collectionRef,
	}).Return([]*AssetRecord{
		bundleAsset(collectionRef, bundleAssetID(20), testKey(t, 55), 1,
			AssetTypeCollectible),
		bundleAsset(collectionRef, bundleAssetID(21), testKey(t, 56), 1,
			AssetTypeCollectible),
	}, nil).Once()

	collections, err := wallet.ListCollections(ctx, req)
	require.NoError(t, err)
	require.Len(t, collections, 1)
	require.Equal(t, collectionRef, collections[0].AssetRef)
	require.Equal(t, uint64(2), collections[0].ItemCount)

	mc.AssertExpectations(t)
}

func TestListAssets_DeduplicatesCollectibleRows(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	assetID := bundleAssetID(23)
	assetRef := AssetRefFromAssetID(assetID)
	req := &ListAssetsRequest{
		AssetRef:     &assetRef,
		IncludeSpent: true,
	}

	mc.On("ListAssetRecords", ctx, req).Return([]*AssetRecord{
		bundleAsset(assetRef, assetID, testKey(t, 59), 1,
			AssetTypeCollectible),
		bundleAsset(assetRef, assetID, testKey(t, 60), 1,
			AssetTypeCollectible),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, assetRef, assets[0].AssetRef)
	require.Equal(t, uint64(1), assets[0].Amount)

	mc.AssertExpectations(t)
}

func TestListAssets_SaturatesFungibleOverflow(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	groupRef := AssetRefFromGroupKey(testKey(t, 61))
	req := &ListAssetsRequest{AssetRef: &groupRef}

	mc.On("ListAssetRecords", ctx, req).Return([]*AssetRecord{
		bundleAsset(groupRef, bundleAssetID(24), testKey(t, 62),
			math.MaxUint64-5, AssetTypeNormal),
		bundleAsset(groupRef, bundleAssetID(25), testKey(t, 63),
			10, AssetTypeNormal),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, uint64(math.MaxUint64), assets[0].Amount)

	mc.AssertExpectations(t)
}

func TestListAssets_FiltersSemanticAmountAfterAggregation(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	groupRef := AssetRefFromGroupKey(testKey(t, 67))
	req := &ListAssetsRequest{
		AssetRef:  &groupRef,
		MinAmount: 300,
		MaxAmount: 400,
	}

	mc.On("ListAssetRecords", ctx, &ListAssetsRequest{
		AssetRef: &groupRef,
	}).Return([]*AssetRecord{
		bundleAsset(groupRef, bundleAssetID(27), testKey(t, 68), 100,
			AssetTypeNormal),
		bundleAsset(groupRef, bundleAssetID(28), testKey(t, 69), 250,
			AssetTypeNormal),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, groupRef, assets[0].AssetRef)
	require.Equal(t, uint64(350), assets[0].Amount)

	mc.AssertExpectations(t)
}

func TestListAssets_FiltersSemanticAmountMiss(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	groupRef := AssetRefFromGroupKey(testKey(t, 70))
	req := &ListAssetsRequest{
		AssetRef:  &groupRef,
		MinAmount: 400,
	}

	mc.On("ListAssetRecords", ctx, &ListAssetsRequest{
		AssetRef: &groupRef,
	}).Return([]*AssetRecord{
		bundleAsset(groupRef, bundleAssetID(29), testKey(t, 71), 100,
			AssetTypeNormal),
		bundleAsset(groupRef, bundleAssetID(30), testKey(t, 72), 250,
			AssetTypeNormal),
	}, nil).Once()

	assets, err := wallet.ListAssets(ctx, req)
	require.NoError(t, err)
	require.Empty(t, assets)

	mc.AssertExpectations(t)
}

func TestListIssuances_SaturatesOverflow(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	groupRef := AssetRefFromGroupKey(testKey(t, 64))
	issuanceID := bundleAssetID(26)
	req := &ListIssuancesRequest{AssetRef: &groupRef}

	mc.On("ListAssetRecords", ctx, &ListAssetsRequest{
		AssetRef: &groupRef,
	}).Return([]*AssetRecord{
		bundleAsset(groupRef, issuanceID, testKey(t, 65),
			math.MaxUint64-5, AssetTypeNormal),
		bundleAsset(groupRef, issuanceID, testKey(t, 66),
			10, AssetTypeNormal),
	}, nil).Once()

	issuances, err := wallet.ListIssuances(ctx, req)
	require.NoError(t, err)
	require.Len(t, issuances, 1)
	require.Equal(t, issuanceID, issuances[0].IssuanceID)
	require.Equal(t, AssetRefFromAssetID(issuanceID),
		issuances[0].Ref())
	require.Equal(t, uint64(math.MaxUint64), issuances[0].Amount)

	mc.AssertExpectations(t)
}

func TestListIssuances_FiltersCollectibles(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	groupRef := AssetRefFromGroupKey(testKey(t, 48))
	nftRef := AssetRefFromAssetID(bundleAssetID(16))

	mc.On("ListAssetRecords", ctx, (*ListAssetsRequest)(nil)).Return(
		[]*AssetRecord{
			bundleAsset(groupRef, bundleAssetID(17), testKey(t, 49),
				100, AssetTypeNormal),
			bundleAsset(nftRef, bundleAssetID(16), testKey(t, 50),
				1, AssetTypeCollectible),
		}, nil,
	).Once()

	issuances, err := wallet.ListIssuances(ctx, nil)
	require.NoError(t, err)
	require.Len(t, issuances, 1)
	require.Equal(t, groupRef, issuances[0].AssetRef)
	require.Equal(t, bundleAssetID(17), issuances[0].IssuanceID)
	require.Equal(t, uint64(100), issuances[0].Amount)

	mc.AssertExpectations(t)
}

func TestListCollectionItems_UsesConcreteItemRefs(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	collectionRef := AssetRefFromGroupKey(testKey(t, 51))
	firstID := bundleAssetID(18)
	secondID := bundleAssetID(19)
	req := &ListCollectionItemsRequest{CollectionRef: &collectionRef}

	mc.On("ListAssetRecords", ctx, &ListAssetsRequest{
		AssetRef: &collectionRef,
	}).Return([]*AssetRecord{
		bundleAsset(collectionRef, firstID, testKey(t, 52), 1,
			AssetTypeCollectible),
		bundleAsset(collectionRef, secondID, testKey(t, 53), 1,
			AssetTypeCollectible),
	}, nil).Once()

	items, err := wallet.ListCollectionItems(ctx, req)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, AssetRefFromAssetID(firstID),
		items[0].AssetRef)
	require.Equal(t, AssetRefFromAssetID(secondID),
		items[1].AssetRef)
	require.NotNil(t, items[0].CollectionRef)
	require.Equal(t, collectionRef, *items[0].CollectionRef)

	mc.AssertExpectations(t)
}

func TestListCollectionItems_ByItemAssetRef(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)
	wallet := NewWallet(mc, NetworkRegtest)

	collectionRef := AssetRefFromGroupKey(testKey(t, 57))
	itemID := bundleAssetID(22)
	itemRef := AssetRefFromAssetID(itemID)
	req := &ListCollectionItemsRequest{AssetRef: &itemRef}

	mc.On("ListAssetRecords", ctx, &ListAssetsRequest{
		AssetRef: &itemRef,
	}).Return([]*AssetRecord{
		bundleAsset(collectionRef, itemID, testKey(t, 58), 1,
			AssetTypeCollectible),
	}, nil).Once()

	items, err := wallet.ListCollectionItems(ctx, req)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, itemRef, items[0].AssetRef)
	require.NotNil(t, items[0].CollectionRef)
	require.Equal(t, collectionRef, *items[0].CollectionRef)

	mc.AssertExpectations(t)
}
