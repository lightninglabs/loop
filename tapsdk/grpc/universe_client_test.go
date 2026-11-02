package grpc

import (
	"testing"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/stretchr/testify/require"
)

// validGroupKey is the compressed secp256k1 generator point G, which is a
// valid public key suitable for tests that call ParsePubKey.
var validGroupKey = []byte{
	0x02,
	0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac,
	0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
	0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9,
	0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98,
}

func TestMarshalUniverseID(t *testing.T) {
	assetID := tapsdk.AssetID{}
	copy(assetID[:], testAssetID)

	groupKey, err := tapsdk.ParsePubKey(validGroupKey)
	require.NoError(t, err)

	tests := []struct {
		name     string
		id       *tapsdk.UniverseID
		wantErr  bool
		validate func(*testing.T, *universerpc.ID)
	}{
		{
			name: "by asset ID",
			id: &tapsdk.UniverseID{
				AssetRef:  tapsdk.AssetRefFromAssetID(assetID),
				ProofType: tapsdk.ProofTypeIssuance,
			},
			validate: func(t *testing.T,
				rpcID *universerpc.ID) {

				require.Equal(
					t, testAssetID,
					rpcID.GetAssetId(),
				)
				require.Equal(
					t,
					universerpc.ProofType_PROOF_TYPE_ISSUANCE,
					rpcID.ProofType,
				)
			},
		},
		{
			name: "by group key",
			id: &tapsdk.UniverseID{
				AssetRef:  tapsdk.AssetRefFromGroupKey(groupKey),
				ProofType: tapsdk.ProofTypeTransfer,
			},
			validate: func(t *testing.T,
				rpcID *universerpc.ID) {

				require.Equal(
					t, validGroupKey,
					rpcID.GetGroupKey(),
				)
				require.Equal(
					t,
					universerpc.ProofType_PROOF_TYPE_TRANSFER,
					rpcID.ProofType,
				)
			},
		},
		{
			name:    testUnspecified,
			id:      &tapsdk.UniverseID{},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcID, err := marshalUniverseID(tc.id)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			tc.validate(t, rpcID)
		})
	}
}

func TestUnmarshalUniverseID(t *testing.T) {
	tests := []struct {
		name     string
		rpcID    *universerpc.ID
		wantErr  string
		validate func(*testing.T, *tapsdk.UniverseID)
	}{
		{
			name:    "nil ID",
			rpcID:   nil,
			wantErr: "nil universe ID",
		},
		{
			name: "by asset ID bytes",
			rpcID: &universerpc.ID{
				Id: &universerpc.ID_AssetId{
					AssetId: testAssetID,
				},
				ProofType: universerpc.ProofType_PROOF_TYPE_ISSUANCE,
			},
			validate: func(t *testing.T,
				id *tapsdk.UniverseID) {

				require.True(t, id.AssetRef.IsAssetIDRef())
				require.Equal(
					t, tapsdk.ProofTypeIssuance,
					id.ProofType,
				)
			},
		},
		{
			name: "by group key bytes",
			rpcID: &universerpc.ID{
				Id: &universerpc.ID_GroupKey{
					GroupKey: validGroupKey,
				},
				ProofType: universerpc.ProofType_PROOF_TYPE_TRANSFER,
			},
			validate: func(t *testing.T,
				id *tapsdk.UniverseID) {

				require.True(t, id.AssetRef.IsGroupRef())
				require.Equal(
					t, tapsdk.ProofTypeTransfer,
					id.ProofType,
				)
			},
		},
		{
			name: "invalid asset ID length",
			rpcID: &universerpc.ID{
				Id: &universerpc.ID_AssetId{
					AssetId: []byte{0x01},
				},
			},
			wantErr: "invalid asset ID",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := unmarshalUniverseID(tc.rpcID)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, id)
			tc.validate(t, id)
		})
	}
}

func TestUnmarshalMerkleSumNode(t *testing.T) {
	tests := []struct {
		name    string
		rpcNode *universerpc.MerkleSumNode
		wantNil bool
		wantErr string
	}{
		{
			name:    "nil node",
			rpcNode: nil,
			wantNil: true,
		},
		{
			name: "valid node",
			rpcNode: &universerpc.MerkleSumNode{
				RootHash: testAssetID,
				RootSum:  42000,
			},
		},
		{
			name: "invalid root hash length",
			rpcNode: &universerpc.MerkleSumNode{
				RootHash: []byte{0x01},
			},
			wantErr: "invalid root hash",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, err := unmarshalMerkleSumNode(tc.rpcNode)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, node)
				return
			}
			require.NotNil(t, node)
			require.Equal(
				t, tc.rpcNode.RootSum, node.RootSum,
			)
		})
	}
}

func TestUnmarshalUniverseRoot(t *testing.T) {
	tests := []struct {
		name    string
		rpcRoot *universerpc.UniverseRoot
		wantErr string
	}{
		{
			name:    "nil root",
			rpcRoot: nil,
			wantErr: "nil universe root",
		},
		{
			name: "valid root with amounts",
			rpcRoot: &universerpc.UniverseRoot{
				Id: &universerpc.ID{
					Id: &universerpc.ID_AssetId{
						AssetId: testAssetID,
					},
				},
				MssmtRoot: &universerpc.MerkleSumNode{
					RootHash: testAssetID,
					RootSum:  100000,
				},
				AssetName: "test-coin",
				AmountsByAssetId: map[string]uint64{
					"deadbeef": 50000,
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := unmarshalUniverseRoot(tc.rpcRoot)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, root)
			require.Equal(
				t, "test-coin", root.AssetName,
			)
			require.NotNil(t, root.MSSMTRoot)
			require.Equal(
				t, int64(100000),
				root.MSSMTRoot.RootSum,
			)
		})
	}
}

func TestUnmarshalAssetLeafKey(t *testing.T) {
	tests := []struct {
		name    string
		rpcKey  *universerpc.AssetKey
		wantErr string
	}{
		{
			name:    "nil key",
			rpcKey:  nil,
			wantErr: "nil asset key",
		},
		{
			name: "with string outpoint and script key bytes",
			rpcKey: &universerpc.AssetKey{
				Outpoint: &universerpc.AssetKey_OpStr{
					OpStr: zeroGenesisPoint,
				},
				ScriptKey: &universerpc.AssetKey_ScriptKeyBytes{
					ScriptKeyBytes: validGroupKey,
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := unmarshalAssetLeafKey(tc.rpcKey)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, key)
		})
	}
}

func TestUnmarshalAssetLeaf(t *testing.T) {
	tests := []struct {
		name    string
		rpcLeaf *universerpc.AssetLeaf
		wantErr string
	}{
		{
			name:    "nil leaf",
			rpcLeaf: nil,
			wantErr: "nil asset leaf",
		},
		{
			name: "leaf with proof only",
			rpcLeaf: &universerpc.AssetLeaf{
				Proof: []byte("raw proof bytes"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leaf, err := unmarshalAssetLeaf(tc.rpcLeaf)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, leaf)
			require.Equal(
				t, tc.rpcLeaf.Proof, leaf.Proof,
			)
		})
	}
}

func TestMarshalUniverseKey(t *testing.T) {
	assetID := tapsdk.AssetID{}
	copy(assetID[:], testAssetID)

	scriptKey, err := tapsdk.ParsePubKey(validGroupKey)
	require.NoError(t, err)

	op, err := tapsdk.NewOutpointFromStr(zeroGenesisPoint)
	require.NoError(t, err)

	key := &tapsdk.UniverseKey{
		ID: tapsdk.UniverseID{
			AssetRef:  tapsdk.AssetRefFromAssetID(assetID),
			ProofType: tapsdk.ProofTypeIssuance,
		},
		LeafKey: tapsdk.AssetLeafKey{
			Outpoint:  op,
			ScriptKey: scriptKey,
		},
	}

	rpcKey, err := marshalUniverseKey(key)
	require.NoError(t, err)
	require.NotNil(t, rpcKey)
	require.NotNil(t, rpcKey.Id)
	require.NotNil(t, rpcKey.LeafKey)
	require.Equal(
		t, testAssetID, rpcKey.Id.GetAssetId(),
	)
}

func TestMarshalProofType(t *testing.T) {
	tests := []struct {
		name string
		in   tapsdk.ProofType
		want universerpc.ProofType
	}{
		{
			name: "issuance",
			in:   tapsdk.ProofTypeIssuance,
			want: universerpc.ProofType_PROOF_TYPE_ISSUANCE,
		},
		{
			name: "transfer",
			in:   tapsdk.ProofTypeTransfer,
			want: universerpc.ProofType_PROOF_TYPE_TRANSFER,
		},
		{
			name: testUnspecified,
			in:   tapsdk.ProofTypeUnspecified,
			want: universerpc.ProofType_PROOF_TYPE_UNSPECIFIED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalProofType(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnmarshalProofType(t *testing.T) {
	tests := []struct {
		name string
		in   universerpc.ProofType
		want tapsdk.ProofType
	}{
		{
			name: "issuance",
			in:   universerpc.ProofType_PROOF_TYPE_ISSUANCE,
			want: tapsdk.ProofTypeIssuance,
		},
		{
			name: "transfer",
			in:   universerpc.ProofType_PROOF_TYPE_TRANSFER,
			want: tapsdk.ProofTypeTransfer,
		},
		{
			name: testUnspecified,
			in:   universerpc.ProofType_PROOF_TYPE_UNSPECIFIED,
			want: tapsdk.ProofTypeUnspecified,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unmarshalProofType(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMarshalSortDirection(t *testing.T) {
	// marshalSortDirection is a direct cast, matching
	// wallet_client.go's approach for consistency.
	got := marshalSortDirection(tapsdk.SortDescending)
	require.Equal(
		t, taprpc.SortDirection(tapsdk.SortDescending), got,
	)

	got = marshalSortDirection(tapsdk.SortAscending)
	require.Equal(
		t, taprpc.SortDirection(tapsdk.SortAscending), got,
	)
}

func TestMarshalAssetStatsQuery(t *testing.T) {
	assetID := tapsdk.AssetID{}
	copy(assetID[:], testAssetID)

	tests := []struct {
		name     string
		req      *tapsdk.AssetStatsQuery
		validate func(*testing.T,
			*universerpc.AssetStatsQuery)
	}{
		{
			name: "full query",
			req: &tapsdk.AssetStatsQuery{
				AssetNameFilter: testTest,
				AssetRefFilter: func() *tapsdk.AssetRef {
					ref := tapsdk.AssetRefFromAssetID(assetID)
					return &ref
				}(),
				AssetTypeFilter: tapsdk.FilterAssetNormal,
				SortBy:          tapsdk.SortByAssetName,
				Offset:          10,
				Limit:           50,
				Direction:       tapsdk.SortDescending,
			},
			validate: func(t *testing.T,
				rpcReq *universerpc.AssetStatsQuery) {

				require.Equal(
					t, testTest,
					rpcReq.AssetNameFilter,
				)
				require.Equal(
					t, testAssetID,
					rpcReq.AssetIdFilter,
				)
				require.Equal(
					t,
					universerpc.AssetTypeFilter_FILTER_ASSET_NORMAL,
					rpcReq.AssetTypeFilter,
				)
				require.Equal(
					t,
					universerpc.AssetQuerySort_SORT_BY_ASSET_NAME,
					rpcReq.SortBy,
				)
				require.Equal(
					t, int32(10), rpcReq.Offset,
				)
				require.Equal(
					t, int32(50), rpcReq.Limit,
				)
			},
		},
		{
			name: "minimal query",
			req:  &tapsdk.AssetStatsQuery{},
			validate: func(t *testing.T,
				rpcReq *universerpc.AssetStatsQuery) {

				require.Empty(
					t, rpcReq.AssetNameFilter,
				)
				require.Nil(t, rpcReq.AssetIdFilter)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq := marshalAssetStatsQuery(tc.req)
			tc.validate(t, rpcReq)
		})
	}
}

func TestUnmarshalAssetStatsSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		rpcSnap *universerpc.AssetStatsSnapshot
		wantErr string
	}{
		{
			name:    "nil snapshot",
			rpcSnap: nil,
			wantErr: "nil stats snapshot",
		},
		{
			name: "snapshot with group key",
			rpcSnap: &universerpc.AssetStatsSnapshot{
				GroupKey:    validGroupKey,
				GroupSupply: 1000000,
				GroupAnchor: &universerpc.AssetStatsAsset{
					AssetId:      testAssetID,
					GenesisPoint: zeroGenesisPoint,
					TotalSupply:  500000,
					AssetName:    "anchor",
					AssetType:    taprpc.AssetType_NORMAL,
				},
				TotalSyncs:  42,
				TotalProofs: 100,
			},
		},
		{
			name: "snapshot without group",
			rpcSnap: &universerpc.AssetStatsSnapshot{
				Asset: &universerpc.AssetStatsAsset{
					AssetId:     testAssetID,
					TotalSupply: 1,
					AssetName:   "collectible",
					AssetType:   taprpc.AssetType_COLLECTIBLE,
				},
				TotalSyncs:  5,
				TotalProofs: 10,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := unmarshalAssetStatsSnapshot(
				tc.rpcSnap,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, snap)
			require.Equal(
				t, tc.rpcSnap.TotalSyncs,
				snap.TotalSyncs,
			)
			require.Equal(
				t, tc.rpcSnap.TotalProofs,
				snap.TotalProofs,
			)
		})
	}
}

func TestUnmarshalAssetStatsAsset(t *testing.T) {
	tests := []struct {
		name     string
		rpcAsset *universerpc.AssetStatsAsset
		wantErr  string
	}{
		{
			name:     "nil asset",
			rpcAsset: nil,
			wantErr:  "nil stats asset",
		},
		{
			name: "normal asset",
			rpcAsset: &universerpc.AssetStatsAsset{
				AssetId:        testAssetID,
				GenesisPoint:   zeroGenesisPoint,
				TotalSupply:    21000000,
				AssetName:      "test-coin",
				AssetType:      taprpc.AssetType_NORMAL,
				GenesisHeight:  800000,
				DecimalDisplay: 8,
			},
		},
		{
			name: "collectible asset",
			rpcAsset: &universerpc.AssetStatsAsset{
				AssetId:     testAssetID,
				TotalSupply: 1,
				AssetName:   "nft",
				AssetType:   taprpc.AssetType_COLLECTIBLE,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asset, err := unmarshalAssetStatsAsset(
				tc.rpcAsset,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, asset)
			require.Equal(
				t, tc.rpcAsset.AssetName,
				asset.AssetName,
			)
			require.Equal(
				t, tc.rpcAsset.TotalSupply,
				asset.TotalSupply,
			)
		})
	}
}
