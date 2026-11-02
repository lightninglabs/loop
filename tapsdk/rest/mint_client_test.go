package rest

import (
	"testing"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/stretchr/testify/require"
)

const (
	testCompressedKey = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce" +
		"28d959f2815b16f81798"
	testAssetID = "000102030405060708090a0b0c0d0e0f1011121314151617" +
		"18191a1b1c1d1e1f"
	testZeroHash = "000000000000000000000000000000000000000000000000" +
		"0000000000000000"
)

func TestMarshalMintAssetJSONIncludesExternalGroupKey(t *testing.T) {
	key := &tapsdk.ExternalKey{
		XPub:           "tpub-external",
		DerivationPath: "m/86'/1'/0'/0/0",
		MasterFingerprint: [4]byte{
			0xde, 0xad, 0xbe, 0xef,
		},
	}

	rpcAsset := marshalMintAssetJSON(&mintAsset{
		AssetType:        tapsdk.AssetTypeFungible,
		Name:             testToken,
		Amount:           100,
		NewGroupedAsset:  true,
		externalGroupKey: key,
	})

	require.NotNil(t, rpcAsset.ExternalGroupKey)
	require.Equal(t, key.XPub, rpcAsset.ExternalGroupKey.XPub)
	require.Equal(t, key.DerivationPath,
		rpcAsset.ExternalGroupKey.DerivationPath)
	require.Equal(t, "deadbeef",
		rpcAsset.ExternalGroupKey.MasterFingerprint)
}

func TestUnmarshalRESTVerboseBatchIncludesUnsealedAssets(t *testing.T) {
	verboseBatch, err := unmarshalRESTVerboseBatch(&jsonVerboseBatch{
		Batch: &jsonMintingBatch{
			BatchKey:  testCompressedKey,
			State:     "BATCH_STATE_PENDING",
			CreatedAt: "1",
		},
		UnsealedAssets: []*jsonUnsealedAsset{
			{
				Asset: &jsonPendingAsset{
					AssetType: assetTypeNormalJSON,
					Name:      testToken,
					Amount:    "100",
					ScriptKey: &jsonScriptKey{
						PubKey: testCompressedKey,
					},
				},
				GroupKeyRequest: &jsonGroupKeyRequest{
					AnchorGenesis: &jsonGenesisInfo{
						GenesisPoint: "txid:0",
						Name:         testToken,
						MetaHash:     testZeroHash,
						AssetID:      testAssetID,
						AssetType:    assetTypeNormalJSON,
					},
					TapscriptRoot: testZeroHash,
					NewAsset:      "abcd",
					ExternalKey: &jsonExternalKey{
						XPub:              "tpub-external",
						MasterFingerprint: "deadbeef",
						DerivationPath:    "m/86'/1'/0'/0/0",
					},
				},
				GroupVirtualTx: &jsonGroupVirtualTx{
					Transaction: "02000000",
					PrevOut: &jsonTxOut{
						Value:    "100",
						PkScript: "5120",
					},
					GenesisID:  testAssetID,
					TweakedKey: testCompressedKey,
				},
				GroupVirtualPSBT: "psbt",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, verboseBatch.UnsealedAssets, 1)

	unsealed := verboseBatch.UnsealedAssets[0]
	require.Equal(t, "psbt", unsealed.GroupVirtualPSBT)
	require.Equal(t, testToken, unsealed.Asset.Name)
	require.Equal(t, uint64(100), unsealed.Asset.Amount)
	require.Equal(t, "tpub-external",
		unsealed.GroupKeyRequest.ExternalKey.XPub)
	require.Equal(t, int64(100), unsealed.GroupVirtualTx.PrevOut.Value)
	require.NotNil(t, unsealed.GroupVirtualTx.TweakedKey)
}
