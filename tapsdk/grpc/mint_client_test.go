package grpc

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"github.com/stretchr/testify/require"
)

func TestMarshalMintAssetRequest(t *testing.T) {
	assetMetaHash := func() tapsdk.Hash {
		var hash tapsdk.Hash
		copy(hash[:], testAssetID)
		return hash
	}()

	tests := []struct {
		name     string
		req      *mintAssetRequest
		validate func(*testing.T, *mintrpc.MintAssetRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T, rpcReq *mintrpc.MintAssetRequest) {
				require.NotNil(t, rpcReq)
				require.Nil(t, rpcReq.Asset)
				require.False(t, rpcReq.ShortResponse)
			},
		},
		{
			name: "full request",
			req: &mintAssetRequest{
				shortResponse: true,
				asset: &mintAsset{
					AssetVersion:    tapsdk.AssetVersionV1,
					AssetType:       tapsdk.AssetTypeNormal,
					Name:            testUsdTest,
					Amount:          42,
					NewGroupedAsset: true,
					AssetMeta: &tapsdk.AssetMeta{
						Data:     []byte(`{"ticker":"USDt"}`),
						Type:     tapsdk.AssetMetaTypeJSON,
						MetaHash: assetMetaHash,
					},
					GroupKey: func() *tapsdk.PubKey {
						var key tapsdk.PubKey
						copy(key[:], testPubKey)
						return &key
					}(),
					GroupAnchor: "anchor-asset",
					GroupInternalKey: &tapsdk.KeyDescriptor{
						RawKeyBytes: func() tapsdk.PubKey {
							var key tapsdk.PubKey
							copy(key[:], testPubKey)
							return key
						}(),
						KeyLocator: tapsdk.KeyLocator{
							Family: 7,
							Index:  9,
						},
					},
					GroupTapscriptRoot: []byte{0xaa, 0xbb},
					ScriptKey: &tapsdk.ScriptKey{
						PubKey: func() tapsdk.PubKey {
							var key tapsdk.PubKey
							copy(key[:], testPubKey)
							return key
						}(),
						TapTweak: []byte{0x01, 0x02},
					},
					groupedAsset:   true,
					decimalDisplay: 2,
					externalGroupKey: &tapsdk.ExternalKey{
						XPub:           "xpub-test",
						DerivationPath: "m/86'/0'/0'/0/7",
						MasterFingerprint: [4]byte{
							0xde, 0xad, 0xbe, 0xef,
						},
					},
					enableSupplyCommitments: true,
				},
			},
			validate: func(t *testing.T, rpcReq *mintrpc.MintAssetRequest) {
				require.True(t, rpcReq.ShortResponse)
				require.NotNil(t, rpcReq.Asset)
				require.Equal(
					t,
					taprpc.AssetVersion_ASSET_VERSION_V1,
					rpcReq.Asset.AssetVersion,
				)
				require.Equal(t, taprpc.AssetType_NORMAL,
					rpcReq.Asset.AssetType)
				require.Equal(t, testUsdTest, rpcReq.Asset.Name)
				require.Equal(t, uint64(42), rpcReq.Asset.Amount)
				require.True(t, rpcReq.Asset.NewGroupedAsset)
				require.True(t, rpcReq.Asset.GroupedAsset)
				require.Equal(t, testPubKey, rpcReq.Asset.GroupKey)
				require.Equal(t, "anchor-asset",
					rpcReq.Asset.GroupAnchor)
				require.Equal(
					t,
					[]byte{0xaa, 0xbb},
					rpcReq.Asset.GroupTapscriptRoot,
				)
				require.NotNil(t, rpcReq.Asset.AssetMeta)
				require.Equal(
					t,
					taprpc.AssetMetaType_META_TYPE_JSON,
					rpcReq.Asset.AssetMeta.Type,
				)
				require.Equal(t, assetMetaHash[:],
					rpcReq.Asset.AssetMeta.MetaHash)
				require.NotNil(t, rpcReq.Asset.GroupInternalKey)
				require.Equal(t, int32(7),
					rpcReq.Asset.GroupInternalKey.KeyLoc.KeyFamily)
				require.NotNil(t, rpcReq.Asset.ScriptKey)
				require.Equal(t, testPubKey,
					rpcReq.Asset.ScriptKey.PubKey)
				require.Equal(t, uint32(2),
					rpcReq.Asset.DecimalDisplay)
				require.NotNil(t, rpcReq.Asset.ExternalGroupKey)
				require.Equal(t, "xpub-test",
					rpcReq.Asset.ExternalGroupKey.Xpub)
				require.True(t,
					rpcReq.Asset.EnableSupplyCommitments)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq := marshalMintAssetRequest(tc.req)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestMarshalFinalizeBatchRequest(t *testing.T) {
	leftHash := func() tapsdk.Hash {
		var hash tapsdk.Hash
		copy(hash[:], testAssetID)
		return hash
	}()
	rightHash := func() tapsdk.Hash {
		var hash tapsdk.Hash
		copy(hash[:], testXOnlyPubKey)
		return hash
	}()

	tests := []struct {
		name     string
		req      *tapsdk.FinalizeBatchRequest
		wantErr  string
		validate func(*testing.T, *mintrpc.FinalizeBatchRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T,
				rpcReq *mintrpc.FinalizeBatchRequest) {

				require.NotNil(t, rpcReq)
				require.False(t, rpcReq.ShortResponse)
				require.Zero(t, rpcReq.FeeRate)
			},
		},
		{
			name: "full tree sibling",
			req: &tapsdk.FinalizeBatchRequest{
				ShortResponse: true,
				FeeRate: tapsdk.NewFeeRateSatPerKVByte(
					2000,
				),
				BatchSibling: &tapsdk.BatchSibling{
					FullTree: &tapsdk.TapscriptFullTree{
						Leaves: []tapsdk.TapLeaf{
							{Script: []byte{0x51}},
							{Script: []byte{0x52}},
						},
					},
				},
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.FinalizeBatchRequest) {

				require.True(t, rpcReq.ShortResponse)
				require.Equal(t, uint32(500), rpcReq.FeeRate)
				require.NotNil(t, rpcReq.GetFullTree())
				require.Len(t, rpcReq.GetFullTree().AllLeaves, 2)
				require.Equal(t, []byte{0x51},
					rpcReq.GetFullTree().AllLeaves[0].Script)
			},
		},
		{
			name: "branch sibling",
			req: &tapsdk.FinalizeBatchRequest{
				BatchSibling: &tapsdk.BatchSibling{
					Branch: &tapsdk.TapBranch{
						LeftTapHash:  leftHash,
						RightTapHash: rightHash,
					},
				},
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.FinalizeBatchRequest) {

				require.NotNil(t, rpcReq.GetBranch())
				require.Equal(t, leftHash[:],
					rpcReq.GetBranch().LeftTaphash)
				require.Equal(t, rightHash[:],
					rpcReq.GetBranch().RightTaphash)
			},
		},
		{
			name: "reject both sibling variants",
			req: &tapsdk.FinalizeBatchRequest{
				BatchSibling: &tapsdk.BatchSibling{
					FullTree: &tapsdk.TapscriptFullTree{},
					Branch:   &tapsdk.TapBranch{},
				},
			},
			wantErr: "batch sibling must set exactly one variant",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalFinalizeBatchRequest(tc.req)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestUnmarshalMintingBatch(t *testing.T) {
	metaHash := append([]byte(nil), testAssetID...)
	_, pubKey := btcec.PrivKeyFromBytes(testAssetID)
	validPubKey := pubKey.SerializeCompressed()
	expectedScriptKey, err := tapsdk.ParseTaprootPubKey(validPubKey)
	require.NoError(t, err)

	tests := []struct {
		name     string
		rpcBatch *mintrpc.MintingBatch
		wantErr  string
		validate func(*testing.T, *tapsdk.MintingBatch)
	}{
		{
			name:     "nil batch",
			rpcBatch: nil,
			wantErr:  "nil minting batch",
		},
		{
			name: "invalid batch key",
			rpcBatch: &mintrpc.MintingBatch{
				BatchKey: []byte{0x01},
			},
			wantErr: "invalid batch key",
		},
		{
			name: testInvalidGroupKey,
			rpcBatch: &mintrpc.MintingBatch{
				Assets: []*mintrpc.PendingAsset{
					{
						Name:     "broken",
						GroupKey: []byte{0x01},
					},
				},
			},
			wantErr: testInvalidGroupKey,
		},
		{
			name: "valid batch",
			rpcBatch: &mintrpc.MintingBatch{
				BatchKey:   append([]byte(nil), validPubKey...),
				BatchTxid:  "abc123",
				State:      mintrpc.BatchState_BATCH_STATE_PENDING,
				CreatedAt:  1234,
				HeightHint: 99,
				BatchPsbt:  []byte{0xaa, 0xbb},
				Assets: []*mintrpc.PendingAsset{
					{
						AssetVersion:    taprpc.AssetVersion_ASSET_VERSION_V1,
						AssetType:       taprpc.AssetType_NORMAL,
						Name:            testUsdTest,
						Amount:          123,
						NewGroupedAsset: true,
						AssetMeta: &taprpc.AssetMeta{
							Data:     []byte(`{"ticker":"USDt"}`),
							Type:     taprpc.AssetMetaType_META_TYPE_JSON,
							MetaHash: metaHash,
						},
						GroupKey:    append([]byte(nil), validPubKey...),
						GroupAnchor: "anchor-asset",
						GroupInternalKey: &taprpc.KeyDescriptor{
							RawKeyBytes: append([]byte(nil), validPubKey...),
							KeyLoc: &taprpc.KeyLocator{
								KeyFamily: 7,
								KeyIndex:  9,
							},
						},
						GroupTapscriptRoot: []byte{0xaa, 0xbb},
						ScriptKey: &taprpc.ScriptKey{
							PubKey:   append([]byte(nil), validPubKey...),
							TapTweak: []byte{0x01, 0x02},
						},
					},
				},
			},
			validate: func(t *testing.T, batch *tapsdk.MintingBatch) {
				require.Equal(t, "abc123", batch.BatchTxid)
				require.Equal(t,
					tapsdk.BatchStatePending, batch.State)
				require.Equal(t, int64(1234), batch.CreatedAt)
				require.Equal(t, uint32(99), batch.HeightHint)
				require.Equal(t, []byte{0xaa, 0xbb}, batch.BatchPSBT)
				require.Equal(t, validPubKey, batch.BatchKey[:])
				require.Len(t, batch.Assets, 1)
				asset := batch.Assets[0]
				require.Equal(t, testUsdTest, asset.Name)
				require.Equal(t,
					tapsdk.AssetVersionV1, asset.AssetVersion)
				require.Equal(t,
					tapsdk.AssetTypeNormal, asset.AssetType)
				require.Equal(t, uint64(123), asset.Amount)
				require.True(t, asset.NewGroupedAsset)
				require.Equal(t, "anchor-asset", asset.GroupAnchor)
				require.NotNil(t, asset.AssetMeta)
				require.Equal(t,
					tapsdk.AssetMetaTypeJSON, asset.AssetMeta.Type)
				require.NotNil(t, asset.GroupKey)
				require.Equal(t, validPubKey, (*asset.GroupKey)[:])
				require.NotNil(t, asset.GroupInternalKey)
				require.Equal(t, uint32(7),
					asset.GroupInternalKey.KeyLocator.Family)
				require.NotNil(t, asset.ScriptKey)
				require.Equal(t, expectedScriptKey[:],
					asset.ScriptKey.PubKey[:])
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			batch, err := unmarshalMintingBatch(tc.rpcBatch)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, batch)
			tc.validate(t, batch)
		})
	}
}

func TestMarshalFundBatchRequest(t *testing.T) {
	leftHash := func() tapsdk.Hash {
		var hash tapsdk.Hash
		copy(hash[:], testAssetID)
		return hash
	}()
	rightHash := func() tapsdk.Hash {
		var hash tapsdk.Hash
		copy(hash[:], testXOnlyPubKey)
		return hash
	}()

	tests := []struct {
		name     string
		req      *tapsdk.FundBatchRequest
		wantErr  string
		validate func(*testing.T, *mintrpc.FundBatchRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T,
				rpcReq *mintrpc.FundBatchRequest) {

				require.NotNil(t, rpcReq)
				require.False(t, rpcReq.ShortResponse)
				require.Zero(t, rpcReq.FeeRate)
			},
		},
		{
			name: "full tree sibling",
			req: &tapsdk.FundBatchRequest{
				ShortResponse: true,
				FeeRate: tapsdk.NewFeeRateSatPerKVByte(
					2000,
				),
				BatchSibling: &tapsdk.BatchSibling{
					FullTree: &tapsdk.TapscriptFullTree{
						Leaves: []tapsdk.TapLeaf{{Script: []byte{0x51}}},
					},
				},
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.FundBatchRequest) {

				require.True(t, rpcReq.ShortResponse)
				require.Equal(t, uint32(500), rpcReq.FeeRate)
				require.NotNil(t, rpcReq.GetFullTree())
				require.Len(t, rpcReq.GetFullTree().AllLeaves, 1)
			},
		},
		{
			name: "branch sibling",
			req: &tapsdk.FundBatchRequest{
				BatchSibling: &tapsdk.BatchSibling{
					Branch: &tapsdk.TapBranch{
						LeftTapHash:  leftHash,
						RightTapHash: rightHash,
					},
				},
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.FundBatchRequest) {

				require.NotNil(t, rpcReq.GetBranch())
				require.Equal(t, leftHash[:],
					rpcReq.GetBranch().LeftTaphash)
				require.Equal(t, rightHash[:],
					rpcReq.GetBranch().RightTaphash)
			},
		},
		{
			name: "reject both sibling variants",
			req: &tapsdk.FundBatchRequest{
				BatchSibling: &tapsdk.BatchSibling{
					FullTree: &tapsdk.TapscriptFullTree{},
					Branch:   &tapsdk.TapBranch{},
				},
			},
			wantErr: "batch sibling must set exactly one variant",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalFundBatchRequest(tc.req)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestMarshalSealBatchRequest(t *testing.T) {
	genesisID := func() tapsdk.AssetID {
		var id tapsdk.AssetID
		copy(id[:], testAssetID)
		return id
	}()

	tests := []struct {
		name     string
		req      *tapsdk.SealBatchRequest
		wantErr  string
		validate func(*testing.T, *mintrpc.SealBatchRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T,
				rpcReq *mintrpc.SealBatchRequest) {

				require.NotNil(t, rpcReq)
				require.False(t, rpcReq.ShortResponse)
			},
		},
		{
			name: "group witnesses",
			req: &tapsdk.SealBatchRequest{
				ShortResponse: true,
				GroupWitnesses: []tapsdk.GroupWitness{{
					GenesisID: genesisID,
					Witness:   [][]byte{{0x01}, {0x02}},
				}},
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.SealBatchRequest) {

				require.True(t, rpcReq.ShortResponse)
				require.Len(t, rpcReq.GroupWitnesses, 1)
				require.Equal(t, genesisID[:],
					rpcReq.GroupWitnesses[0].GenesisId)
				require.Len(t, rpcReq.GroupWitnesses[0].Witness, 2)
			},
		},
		{
			name: "signed virtual psbts",
			req: &tapsdk.SealBatchRequest{
				SignedGroupVirtualPSBTs: []string{testPsbtA, "psbt-b"},
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.SealBatchRequest) {

				require.Equal(t, []string{testPsbtA, "psbt-b"},
					rpcReq.SignedGroupVirtualPsbts)
			},
		},
		{
			name: "reject both witness inputs",
			req: &tapsdk.SealBatchRequest{
				GroupWitnesses:          []tapsdk.GroupWitness{{GenesisID: genesisID}},
				SignedGroupVirtualPSBTs: []string{testPsbtA},
			},
			wantErr: "seal batch request must choose one witness input",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalSealBatchRequest(tc.req)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestMarshalListBatchesRequest(t *testing.T) {
	batchKey := func() tapsdk.PubKey {
		var key tapsdk.PubKey
		copy(key[:], testPubKey)
		return key
	}()

	tests := []struct {
		name     string
		req      *tapsdk.ListBatchesRequest
		validate func(*testing.T, *mintrpc.ListBatchRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T,
				rpcReq *mintrpc.ListBatchRequest) {

				require.NotNil(t, rpcReq)
				require.False(t, rpcReq.Verbose)
				require.Nil(t, rpcReq.Filter)
			},
		},
		{
			name: "full request",
			req: &tapsdk.ListBatchesRequest{
				BatchKey: &batchKey,
				Verbose:  true,
			},
			validate: func(t *testing.T,
				rpcReq *mintrpc.ListBatchRequest) {

				require.True(t, rpcReq.Verbose)
				require.Equal(t, batchKey[:], rpcReq.GetBatchKey())
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq := marshalListBatchesRequest(tc.req)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestUnmarshalVerboseMintingBatch(t *testing.T) {
	metaHash := append([]byte(nil), testAssetID...)
	_, pubKey := btcec.PrivKeyFromBytes(testAssetID)
	validPubKey := pubKey.SerializeCompressed()

	rpcBatch := &mintrpc.VerboseBatch{
		Batch: &mintrpc.MintingBatch{
			BatchKey:   append([]byte(nil), validPubKey...),
			BatchTxid:  "funded-batch",
			State:      mintrpc.BatchState_BATCH_STATE_COMMITTED,
			CreatedAt:  77,
			HeightHint: 21,
			BatchPsbt:  []byte{0xaa},
		},
		UnsealedAssets: []*mintrpc.UnsealedAsset{{
			Asset: &mintrpc.PendingAsset{
				AssetVersion: taprpc.AssetVersion_ASSET_VERSION_V1,
				AssetType:    taprpc.AssetType_NORMAL,
				Name:         testUsdTest,
				AssetMeta: &taprpc.AssetMeta{
					Data:     []byte(`{"ticker":"USDt"}`),
					Type:     taprpc.AssetMetaType_META_TYPE_JSON,
					MetaHash: metaHash,
				},
				Amount: 42,
			},
			GroupKeyRequest: &taprpc.GroupKeyRequest{
				AnchorGenesis: &taprpc.GenesisInfo{
					GenesisPoint: "txid:0",
					Name:         testUsdTest,
					MetaHash:     metaHash,
					AssetId:      append([]byte(nil), testAssetID...),
					AssetType:    taprpc.AssetType_NORMAL,
					OutputIndex:  1,
				},
				NewAsset:      []byte{0x01, 0x02},
				TapscriptRoot: []byte{0xab},
				ExternalKey: &taprpc.ExternalKey{
					Xpub:              "xpub-test",
					MasterFingerprint: []byte{0xde, 0xad, 0xbe, 0xef},
					DerivationPath:    "m/86'/0'/0'/0/7",
				},
			},
			GroupVirtualTx: &taprpc.GroupVirtualTx{
				Transaction: append([]byte(nil), []byte{0x11, 0x22}...),
				PrevOut: &taprpc.TxOut{
					Value:    123,
					PkScript: []byte{0x51},
				},
				GenesisId:  append([]byte(nil), testAssetID...),
				TweakedKey: append([]byte(nil), validPubKey...),
			},
			GroupVirtualPsbt: "cHNidP8BAHECAAAAAQ==",
		}},
	}

	batch, err := unmarshalVerboseMintingBatch(rpcBatch)
	require.NoError(t, err)
	require.NotNil(t, batch)
	require.Equal(t, "funded-batch", batch.Batch.BatchTxid)
	require.Equal(t, tapsdk.BatchStateCommitted, batch.Batch.State)
	require.Len(t, batch.UnsealedAssets, 1)
	require.Equal(t, testUsdTest, batch.UnsealedAssets[0].Asset.Name)
	require.Equal(t, "txid:0",
		batch.UnsealedAssets[0].GroupKeyRequest.AnchorGenesis.GenesisPoint)
	require.Equal(t, int64(123),
		batch.UnsealedAssets[0].GroupVirtualTx.PrevOut.Value)
	require.Equal(t, "cHNidP8BAHECAAAAAQ==",
		batch.UnsealedAssets[0].GroupVirtualPSBT)
}
