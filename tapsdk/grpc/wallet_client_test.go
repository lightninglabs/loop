package grpc

import (
	"encoding/hex"
	"testing"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
)

const zeroGenesisPoint = "00000000000000000000000000000000" +
	"00000000000000000000000000000000:1"

func TestMarshalListAssetRecordsRequest(t *testing.T) {
	var assetID tapsdk.AssetID
	copy(assetID[:], testAssetID)
	assetIDRef := tapsdk.AssetRefFromAssetID(assetID)

	var groupPubKey tapsdk.PubKey
	copy(groupPubKey[:], testPubKey)
	groupKeyRef := tapsdk.AssetRefFromGroupKey(groupPubKey)

	explicitType := tapsdk.ScriptKeyTypeBurn
	invalidType := tapsdk.ScriptKeyType(99)
	anchor := tapsdk.Outpoint{
		Txid:  assetID,
		Index: 7,
	}

	tests := []struct {
		name     string
		req      *tapsdk.ListAssetsRequest
		wantErr  string
		validate func(*testing.T, *taprpc.ListAssetRequest,
			*tapsdk.AssetRef)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T, rpcReq *taprpc.ListAssetRequest,
				ref *tapsdk.AssetRef) {

				require.NotNil(t, rpcReq)
				require.Nil(t, ref)
				require.Empty(t, rpcReq.GroupKey)
				require.Nil(t, rpcReq.AnchorOutpoint)
			},
		},
		{
			name: "all protocol filters",
			req: &tapsdk.ListAssetsRequest{
				WithWitness:             true,
				IncludeSpent:            true,
				IncludeLeased:           true,
				IncludeUnconfirmedMints: true,
				MinAmount:               7,
				MaxAmount:               11,
				AssetRef:                &groupKeyRef,
				AnchorOutpoint:          &anchor,
				ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{
					ExplicitType: &explicitType,
				},
			},
			validate: func(t *testing.T, rpcReq *taprpc.ListAssetRequest,
				ref *tapsdk.AssetRef) {

				require.True(t, rpcReq.WithWitness)
				require.True(t, rpcReq.IncludeSpent)
				require.True(t, rpcReq.IncludeLeased)
				require.True(t, rpcReq.IncludeUnconfirmedMints)
				require.Equal(t, uint64(7), rpcReq.MinAmount)
				require.Equal(t, uint64(11), rpcReq.MaxAmount)
				require.Equal(t, groupPubKey[:], rpcReq.GroupKey)
				require.NotNil(t, rpcReq.AnchorOutpoint)
				require.Equal(
					t, assetID[:],
					rpcReq.AnchorOutpoint.Txid,
				)
				require.Equal(
					t, uint32(7),
					rpcReq.AnchorOutpoint.OutputIndex,
				)
				require.Equal(t, &groupKeyRef, ref)
				require.NotNil(t, rpcReq.ScriptKeyType)
				require.Equal(
					t,
					taprpc.ScriptKeyType_SCRIPT_KEY_BURN,
					rpcReq.ScriptKeyType.GetExplicitType(),
				)
			},
		},
		{
			name: "asset ID ref uses local filter only",
			req: &tapsdk.ListAssetsRequest{
				AssetRef: &assetIDRef,
			},
			validate: func(t *testing.T, rpcReq *taprpc.ListAssetRequest,
				ref *tapsdk.AssetRef) {

				require.Empty(t, rpcReq.GroupKey)
				require.Equal(t, &assetIDRef, ref)
			},
		},
		{
			name: "unknown script key type",
			req: &tapsdk.ListAssetsRequest{
				ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{
					ExplicitType: &invalidType,
				},
			},
			wantErr: "unknown script key type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, ref, err := marshalListAssetRecordsRequest(tc.req)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			tc.validate(t, rpcReq, ref)
		})
	}
}

func TestMarshalListBalancesRequest(t *testing.T) {
	var assetID tapsdk.AssetID
	copy(assetID[:], testAssetID)
	assetIDRef := tapsdk.AssetRefFromAssetID(assetID)

	var groupPubKey tapsdk.PubKey
	copy(groupPubKey[:], testPubKey)
	groupKeyRef := tapsdk.AssetRefFromGroupKey(groupPubKey)

	explicitType := tapsdk.ScriptKeyTypeBurn

	tests := []struct {
		name     string
		req      *tapsdk.ListBalancesRequest
		validate func(*testing.T, *taprpc.ListBalancesRequest)
	}{
		{
			name: "nil request groups by asset_id",
			req:  nil,
			validate: func(t *testing.T,
				rpcReq *taprpc.ListBalancesRequest) {

				require.True(
					t, rpcReq.GetAssetId(),
				)
				require.Empty(t, rpcReq.AssetFilter)
				require.Empty(t, rpcReq.GroupKeyFilter)
				require.Nil(t, rpcReq.ScriptKeyType)
			},
		},
		{
			name: "asset ID ref sets asset filter",
			req: &tapsdk.ListBalancesRequest{
				AssetRef: &assetIDRef,
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.ListBalancesRequest) {

				require.True(
					t, rpcReq.GetAssetId(),
				)
				require.Equal(
					t, assetID[:],
					rpcReq.AssetFilter,
				)
				require.Empty(
					t, rpcReq.GroupKeyFilter,
				)
			},
		},
		{
			// Group refs take a separate code path in
			// ListBalances, so the asset-id marshaller
			// produces no filter for them.
			name: "group key ref leaves filters unset",
			req: &tapsdk.ListBalancesRequest{
				AssetRef: &groupKeyRef,
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.ListBalancesRequest) {

				require.True(
					t, rpcReq.GetAssetId(),
				)
				require.Empty(t, rpcReq.AssetFilter)
				require.Empty(t, rpcReq.GroupKeyFilter)
			},
		},
		{
			name: "explicit script key type",
			req: &tapsdk.ListBalancesRequest{
				IncludeLeased: true,
				ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{
					ExplicitType: &explicitType,
				},
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.ListBalancesRequest) {

				require.True(t, rpcReq.IncludeLeased)
				require.NotNil(
					t, rpcReq.ScriptKeyType,
				)
				require.Equal(
					t,
					taprpc.ScriptKeyType_SCRIPT_KEY_BURN,
					rpcReq.ScriptKeyType.GetExplicitType(),
				)
			},
		},
		{
			name: "all script key types",
			req: &tapsdk.ListBalancesRequest{
				ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{
					AllTypes: true,
				},
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.ListBalancesRequest) {

				require.NotNil(
					t, rpcReq.ScriptKeyType,
				)
				require.True(
					t,
					rpcReq.ScriptKeyType.GetAllTypes(),
				)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalListBalancesRequest(tc.req)
			require.NoError(t, err)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestAssetRecordMatchesRef(t *testing.T) {
	var assetID tapsdk.AssetID
	copy(assetID[:], testAssetID)
	itemRef := tapsdk.AssetRefFromAssetID(assetID)

	var groupKey tapsdk.PubKey
	copy(groupKey[:], testPubKey)
	collectionRef := tapsdk.AssetRefFromGroupKey(groupKey)

	var otherID tapsdk.AssetID
	copy(otherID[:], testAssetID)
	otherID[0] ^= 0xff
	otherRef := tapsdk.AssetRefFromAssetID(otherID)

	record := &tapsdk.AssetRecord{
		AssetRef: collectionRef,
		Genesis: tapsdk.IssuanceGenesis{
			IssuanceID: assetID,
		},
	}

	tests := []struct {
		name   string
		record *tapsdk.AssetRecord
		ref    tapsdk.AssetRef
		want   bool
	}{
		{
			name:   "nil record",
			record: nil,
			ref:    collectionRef,
		},
		{
			name:   "group ref matches canonical asset ref",
			record: record,
			ref:    collectionRef,
			want:   true,
		},
		{
			name:   "issuance ID ref matches grouped record",
			record: record,
			ref:    itemRef,
			want:   true,
		},
		{
			name:   "unrelated asset ID ref",
			record: record,
			ref:    otherRef,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(
				t, tc.want,
				assetRecordMatchesRef(tc.record, tc.ref),
			)
		})
	}
}

func TestMarshalSendAssetRequest(t *testing.T) {
	tests := []struct {
		name     string
		req      *tapsdk.SendAssetRequest
		wantErr  error
		validate func(*testing.T, *taprpc.SendAssetRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T, rpcReq *taprpc.SendAssetRequest) {
				require.NotNil(t, rpcReq)
				require.Empty(t, rpcReq.TapAddrs)
				require.Empty(t, rpcReq.AddressesWithAmounts)
			},
		},
		{
			name: "embedded amount routes via TapAddrs",
			req: &tapsdk.SendAssetRequest{
				Recipients: []tapsdk.Recipient{
					tapsdk.RecipientWithEmbeddedAmount(
						"tap1first",
					),
					tapsdk.RecipientWithEmbeddedAmount(
						"tap1second",
					),
				},
				FeeRate: tapsdk.NewFeeRateSatPerKVByte(2000),
				Label:   "batch-send",
			},
			validate: func(t *testing.T, rpcReq *taprpc.SendAssetRequest) {
				require.Equal(
					t,
					[]string{"tap1first", "tap1second"},
					rpcReq.TapAddrs,
				)
				require.Equal(t, uint32(500), rpcReq.FeeRate)
				require.Equal(t, "batch-send", rpcReq.Label)
				require.Empty(t, rpcReq.AddressesWithAmounts)
			},
		},
		{
			name: "explicit Amount routes via AddressesWithAmounts",
			req: &tapsdk.SendAssetRequest{
				Recipients: []tapsdk.Recipient{
					tapsdk.RecipientWithAmount(
						"tap1amountless", 150,
					),
					tapsdk.RecipientWithAmount(
						"tap1fixed", 42,
					),
				},
				SkipProofCourierPingCheck: true,
			},
			validate: func(t *testing.T, rpcReq *taprpc.SendAssetRequest) {
				require.Empty(t, rpcReq.TapAddrs)
				require.True(t, rpcReq.SkipProofCourierPingCheck)
				require.Len(t, rpcReq.AddressesWithAmounts, 2)
				require.Equal(
					t,
					"tap1amountless",
					rpcReq.AddressesWithAmounts[0].TapAddr,
				)
				require.Equal(
					t,
					uint64(150),
					rpcReq.AddressesWithAmounts[0].Amount,
				)
				require.Equal(
					t,
					"tap1fixed",
					rpcReq.AddressesWithAmounts[1].TapAddr,
				)
				require.Equal(
					t,
					uint64(42),
					rpcReq.AddressesWithAmounts[1].Amount,
				)
			},
		},
		{
			name: "mixed Amount rejected",
			req: &tapsdk.SendAssetRequest{
				Recipients: []tapsdk.Recipient{
					tapsdk.RecipientWithAmount(
						"tap1explicit", 50,
					),
					tapsdk.RecipientWithEmbeddedAmount(
						"tap1embedded",
					),
				},
			},
			wantErr: tapsdk.ErrMixedRecipientAmounts,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalSendAssetRequest(tc.req)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestUnmarshalBalance(t *testing.T) {
	tests := []struct {
		name         string
		rpcBalance   *taprpc.AssetBalance
		wantErr      string
		wantGroupRef bool
	}{
		{
			name:       "nil balance",
			rpcBalance: nil,
			wantErr:    "nil asset balance",
		},
		{
			name: "missing genesis",
			rpcBalance: &taprpc.AssetBalance{
				Balance: 10,
			},
			wantErr: "missing asset genesis",
		},
		{
			name: "invalid group key length",
			rpcBalance: &taprpc.AssetBalance{
				AssetGenesis: &taprpc.GenesisInfo{
					GenesisPoint: zeroGenesisPoint,
					Name:         testTest,
					AssetId:      testAssetID,
					OutputIndex:  1,
					AssetType:    taprpc.AssetType_NORMAL,
				},
				GroupKey: []byte{0x01},
			},
			wantErr: "invalid group key length",
		},
		{
			name: "valid grouped asset balance",
			rpcBalance: &taprpc.AssetBalance{
				Balance:  42,
				GroupKey: testPubKey,
			},
			wantGroupRef: true,
		},
		{
			name: "valid ungrouped asset balance",
			rpcBalance: &taprpc.AssetBalance{
				AssetGenesis: &taprpc.GenesisInfo{
					GenesisPoint: zeroGenesisPoint,
					Name:         testTest,
					AssetId:      testAssetID,
					OutputIndex:  1,
					AssetType:    taprpc.AssetType_NORMAL,
				},
				Balance: 42,
			},
		},
		{
			name: testUnknownAssetType,
			rpcBalance: &taprpc.AssetBalance{
				AssetGenesis: &taprpc.GenesisInfo{
					GenesisPoint: zeroGenesisPoint,
					Name:         testTest,
					AssetId:      testAssetID,
					OutputIndex:  1,
					AssetType:    taprpc.AssetType(99),
				},
				Balance: 42,
			},
			wantErr: testUnknownAssetType,
		},
		{
			name: "collection item balance uses asset ID",
			rpcBalance: &taprpc.AssetBalance{
				AssetGenesis: &taprpc.GenesisInfo{
					GenesisPoint: zeroGenesisPoint,
					Name:         "test-nft",
					AssetId:      testAssetID,
					OutputIndex:  1,
					AssetType:    taprpc.AssetType_COLLECTIBLE,
				},
				Balance:  42,
				GroupKey: testPubKey,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			balance, err := unmarshalBalance(tc.rpcBalance)

			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, balance)
			require.Equal(t, uint64(42), balance.Balance)
			if tc.wantGroupRef {
				groupKey, ok := balance.AssetRef.GroupKey()
				require.True(t, ok)
				require.Equal(t, testPubKey, groupKey[:])
			} else {
				assetID, ok := balance.AssetRef.AssetID()
				require.True(t, ok)
				require.Equal(t, testAssetID, assetID[:])
			}
		})
	}
}

func TestUnmarshalAssetTransferGroupKey(t *testing.T) {
	rpcTransfer := &taprpc.AssetTransfer{
		Inputs: []*taprpc.TransferInput{{
			AnchorPoint: zeroGenesisPoint,
			AssetId:     testAssetID,
			ScriptKey:   testPubKey,
			Amount:      100,
			GroupKey:    testPubKey,
			AssetType:   taprpc.AssetType_NORMAL,
		}},
		Outputs: []*taprpc.TransferOutput{{
			Amount:    60,
			AssetId:   testAssetID,
			ScriptKey: testPubKey,
			Anchor: &taprpc.TransferOutputAnchor{
				Outpoint: zeroGenesisPoint,
				Value:    330,
			},
			GroupKey:  testPubKey,
			AssetType: taprpc.AssetType_NORMAL,
		}},
	}

	transfer, err := unmarshalAssetTransfer(rpcTransfer)
	require.NoError(t, err)
	require.Len(t, transfer.Inputs, 1)
	require.Len(t, transfer.Outputs, 1)

	require.NotNil(t, transfer.Inputs[0].GroupKey)
	require.Equal(t, testPubKey, transfer.Inputs[0].GroupKey[:])
	require.Equal(t,
		tapsdk.AssetTypeNormal, transfer.Inputs[0].AssetType)
	require.NotNil(t, transfer.Outputs[0].GroupKey)
	require.Equal(t, testPubKey, transfer.Outputs[0].GroupKey[:])
	require.Equal(t,
		tapsdk.AssetTypeNormal, transfer.Outputs[0].AssetType)
}

func TestUnmarshalAssetTransferCollectionItem(t *testing.T) {
	rpcTransfer := &taprpc.AssetTransfer{
		Inputs: []*taprpc.TransferInput{{
			AnchorPoint: zeroGenesisPoint,
			AssetId:     testAssetID,
			ScriptKey:   testPubKey,
			Amount:      1,
			GroupKey:    testPubKey,
			AssetType:   taprpc.AssetType_COLLECTIBLE,
		}},
		Outputs: []*taprpc.TransferOutput{{
			Amount:    1,
			AssetId:   testAssetID,
			ScriptKey: testPubKey,
			Anchor: &taprpc.TransferOutputAnchor{
				Outpoint: zeroGenesisPoint,
				Value:    330,
			},
			GroupKey:  testPubKey,
			AssetType: taprpc.AssetType_COLLECTIBLE,
		}},
	}

	transfer, err := unmarshalAssetTransfer(rpcTransfer)
	require.NoError(t, err)

	highLevel := tapsdk.NewTransfer(transfer)
	wantRef := tapsdk.AssetRefFromAssetID(func() tapsdk.AssetID {
		var id tapsdk.AssetID
		copy(id[:], testAssetID)
		return id
	}())
	require.True(t, highLevel.Inputs[0].AssetRef.Equivalent(wantRef))
	require.True(t, highLevel.Outputs[0].AssetRef.Equivalent(wantRef))
	require.Equal(t,
		tapsdk.AssetTypeCollectible, highLevel.Inputs[0].Type)
}

func TestUnmarshalAssetTransferUnknownAssetType(t *testing.T) {
	tests := []struct {
		name     string
		transfer *taprpc.AssetTransfer
		wantErr  string
	}{
		{
			name: "unknown output asset type",
			transfer: &taprpc.AssetTransfer{
				Outputs: []*taprpc.TransferOutput{{
					Amount:    1,
					AssetType: taprpc.AssetType(99),
				}},
			},
			wantErr: "invalid output asset type",
		},
		{
			name: "unknown input asset type",
			transfer: &taprpc.AssetTransfer{
				Inputs: []*taprpc.TransferInput{{
					AnchorPoint: zeroGenesisPoint,
					AssetId:     testAssetID,
					ScriptKey:   testPubKey,
					Amount:      1,
					AssetType:   taprpc.AssetType(99),
				}},
			},
			wantErr: "invalid input asset type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unmarshalAssetTransfer(tc.transfer)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestScriptKeyTypeConstants(t *testing.T) {
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_UNKNOWN),
		int(tapsdk.ScriptKeyTypeUnknown),
	)
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_BIP86),
		int(tapsdk.ScriptKeyTypeBIP86),
	)
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_SCRIPT_PATH_EXTERNAL),
		int(tapsdk.ScriptKeyTypeScriptPathExternal),
	)
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_BURN),
		int(tapsdk.ScriptKeyTypeBurn),
	)
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_TOMBSTONE),
		int(tapsdk.ScriptKeyTypeTombstone),
	)
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_CHANNEL),
		int(tapsdk.ScriptKeyTypeChannel),
	)
	require.Equal(t,
		int(taprpc.ScriptKeyType_SCRIPT_KEY_UNIQUE_PEDERSEN),
		int(tapsdk.ScriptKeyTypeUniquePedersen),
	)
}

// validPubKeyBytes is the compressed secp256k1 generator point G.
// Tests that call ParsePubKey need a point actually on the curve.
var validPubKeyBytes = []byte{
	0x02,
	0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac,
	0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
	0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9,
	0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98,
}

func TestUnmarshalManagedUtxo(t *testing.T) {
	tests := []struct {
		name    string
		rpcUtxo *taprpc.ManagedUtxo
		wantErr string
	}{
		{
			name:    "nil utxo",
			rpcUtxo: nil,
			wantErr: "nil managed utxo",
		},
		{
			name: "valid utxo without assets",
			rpcUtxo: &taprpc.ManagedUtxo{
				OutPoint:         zeroGenesisPoint,
				AmtSat:           100000,
				InternalKey:      validPubKeyBytes,
				TaprootAssetRoot: testAssetID,
				MerkleRoot:       testAssetID,
			},
		},
		{
			name: "invalid outpoint",
			rpcUtxo: &taprpc.ManagedUtxo{
				OutPoint:    "invalid",
				InternalKey: validPubKeyBytes,
			},
			wantErr: "invalid outpoint",
		},
		{
			name: "invalid internal key",
			rpcUtxo: &taprpc.ManagedUtxo{
				OutPoint:    zeroGenesisPoint,
				InternalKey: []byte{0x01},
			},
			wantErr: "invalid internal key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := unmarshalManagedUtxo(tc.rpcUtxo)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(
				t, tc.rpcUtxo.AmtSat, result.AmtSat,
			)
		})
	}
}

func TestUnmarshalAssetGroupRecord(t *testing.T) {
	compressedHex := hex.EncodeToString(testPubKey)
	xOnlyHex := hex.EncodeToString(testPubKey[1:])

	tests := []struct {
		name        string
		groupKeyHex string
		rpcGroup    *taprpc.GroupedAssets
		wantErr     string
	}{
		{
			name:        "nil group",
			groupKeyHex: compressedHex,
			rpcGroup:    nil,
			wantErr:     "nil grouped assets",
		},
		{
			name:        "invalid group key hex",
			groupKeyHex: "nothex",
			rpcGroup:    &taprpc.GroupedAssets{},
			wantErr:     testInvalidGroupKey,
		},
		{
			name:        "compressed group key",
			groupKeyHex: compressedHex,
			rpcGroup: &taprpc.GroupedAssets{
				Assets: []*taprpc.AssetHumanReadable{
					{
						Id:       testAssetID,
						Amount:   1000,
						Tag:      "test-asset",
						MetaHash: testAssetID,
						Type:     taprpc.AssetType_NORMAL,
					},
				},
			},
		},
		{
			name:        "x-only group key",
			groupKeyHex: xOnlyHex,
			rpcGroup: &taprpc.GroupedAssets{
				Assets: []*taprpc.AssetHumanReadable{
					{
						Id:       testAssetID,
						Amount:   1000,
						Tag:      "test-asset",
						MetaHash: testAssetID,
						Type:     taprpc.AssetType_NORMAL,
					},
				},
			},
		},
		{
			name:        "nil asset in group",
			groupKeyHex: compressedHex,
			rpcGroup: &taprpc.GroupedAssets{
				Assets: []*taprpc.AssetHumanReadable{nil},
			},
			wantErr: "nil asset in group",
		},
		{
			name:        testUnknownAssetType,
			groupKeyHex: compressedHex,
			rpcGroup: &taprpc.GroupedAssets{
				Assets: []*taprpc.AssetHumanReadable{{
					Id:       testAssetID,
					MetaHash: testAssetID,
					Type:     taprpc.AssetType(99),
				}},
			},
			wantErr: testUnknownAssetType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := unmarshalAssetGroupRecord(
				tc.groupKeyHex, tc.rpcGroup,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.AssetRef.IsGroupRef())
			require.Len(
				t, result.Members,
				len(tc.rpcGroup.Assets),
			)
		})
	}
}

func TestUnmarshalBurnRecord(t *testing.T) {
	var (
		assetID  tapsdk.AssetID
		groupKey tapsdk.PubKey
	)
	copy(assetID[:], testAssetID)
	copy(groupKey[:], validPubKeyBytes)
	collectionRef := tapsdk.AssetRefFromGroupKey(groupKey)

	tests := []struct {
		name     string
		rpcBurn  *taprpc.AssetBurn
		wantRef  tapsdk.AssetRef
		wantColl *tapsdk.AssetRef
		wantType tapsdk.AssetType
		wantErr  string
	}{
		{
			name:    "nil burn",
			rpcBurn: nil,
			wantErr: "nil asset burn",
		},
		{
			name: "valid burn without group key",
			rpcBurn: &taprpc.AssetBurn{
				Note:       "test burn",
				AssetId:    testAssetID,
				Amount:     500,
				AnchorTxid: testAssetID,
			},
			wantRef:  tapsdk.AssetRefFromAssetID(assetID),
			wantType: tapsdk.AssetTypeFungible,
		},
		{
			name: "fungible burn with group key",
			rpcBurn: &taprpc.AssetBurn{
				AssetId:         testAssetID,
				TweakedGroupKey: validPubKeyBytes,
				Amount:          100,
				AnchorTxid:      testAssetID,
				AssetType:       taprpc.AssetType_NORMAL,
			},
			wantRef:  tapsdk.AssetRefFromGroupKey(groupKey),
			wantType: tapsdk.AssetTypeFungible,
		},
		{
			name: "standalone collection item burn",
			rpcBurn: &taprpc.AssetBurn{
				AssetId:    testAssetID,
				Amount:     1,
				AnchorTxid: testAssetID,
				AssetType:  taprpc.AssetType_COLLECTIBLE,
			},
			wantRef:  tapsdk.AssetRefFromAssetID(assetID),
			wantType: tapsdk.AssetTypeNFT,
		},
		{
			name: "collection item burn with group key",
			rpcBurn: &taprpc.AssetBurn{
				AssetId:         testAssetID,
				TweakedGroupKey: validPubKeyBytes,
				Amount:          1,
				AnchorTxid:      testAssetID,
				AssetType:       taprpc.AssetType_COLLECTIBLE,
			},
			wantRef:  tapsdk.AssetRefFromAssetID(assetID),
			wantColl: &collectionRef,
			wantType: tapsdk.AssetTypeNFT,
		},
		{
			name: testInvalidGroupKey,
			rpcBurn: &taprpc.AssetBurn{
				AssetId:         testAssetID,
				TweakedGroupKey: []byte{0x02, 0x03},
				Amount:          100,
				AnchorTxid:      testAssetID,
				AssetType:       taprpc.AssetType_NORMAL,
			},
			wantErr: "invalid tweaked group key",
		},
		{
			name: "invalid collection item amount",
			rpcBurn: &taprpc.AssetBurn{
				AssetId:    testAssetID,
				Amount:     2,
				AnchorTxid: testAssetID,
				AssetType:  taprpc.AssetType_COLLECTIBLE,
			},
			wantErr: "invalid collectible burn amount",
		},
		{
			name: testUnknownAssetType,
			rpcBurn: &taprpc.AssetBurn{
				AssetId:    testAssetID,
				Amount:     100,
				AnchorTxid: testAssetID,
				AssetType:  taprpc.AssetType(99),
			},
			wantErr: "unknown burn asset type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := unmarshalBurnRecord(tc.rpcBurn)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tc.rpcBurn.Amount, result.Amount)
			require.Equal(t, tc.wantRef, result.AssetRef)
			require.Equal(t, tc.wantColl, result.CollectionRef)
			require.Equal(t, tc.wantType, result.Type)
		})
	}
}

func TestMarshalBurnAssetRequest(t *testing.T) {
	assetID := tapsdk.AssetID{}
	copy(assetID[:], testAssetID)

	tests := []struct {
		name     string
		req      *tapsdk.BurnAssetRequest
		wantErr  string
		validate func(*testing.T, *taprpc.BurnAssetRequest)
	}{
		{
			name: "burn by asset ID ref",
			req: &tapsdk.BurnAssetRequest{
				AssetRef:         tapsdk.AssetRefFromAssetID(assetID),
				AmountToBurn:     100,
				ConfirmationText: testAssetsWillBeDestroyed,
				Note:             testTest,
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.BurnAssetRequest) {

				require.NotNil(t, rpcReq.AssetSpecifier)
				require.Equal(
					t, testAssetID,
					rpcReq.AssetSpecifier.GetAssetId(),
				)
				require.Equal(
					t, uint64(100),
					rpcReq.AmountToBurn,
				)
			},
		},
		{
			name: "burn by group key ref",
			req: &tapsdk.BurnAssetRequest{
				AssetRef: func() tapsdk.AssetRef {
					var gk tapsdk.PubKey
					copy(gk[:], testPubKey)
					return tapsdk.AssetRefFromGroupKey(gk)
				}(),
				AmountToBurn:     200,
				ConfirmationText: testAssetsWillBeDestroyed,
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.BurnAssetRequest) {

				require.NotNil(t, rpcReq.AssetSpecifier)
				require.Equal(
					t, testPubKey,
					rpcReq.AssetSpecifier.GetGroupKey(),
				)
				require.Equal(
					t, uint64(200),
					rpcReq.AmountToBurn,
				)
			},
		},
		{
			name: "missing asset ref",
			req: &tapsdk.BurnAssetRequest{
				AmountToBurn:     50,
				ConfirmationText: testAssetsWillBeDestroyed,
			},
			wantErr: "asset ref is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalBurnAssetRequest(tc.req)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			tc.validate(t, rpcReq)
		})
	}
}

func TestMarshalFetchAssetMetaRequest(t *testing.T) {
	assetID := tapsdk.AssetID{}
	copy(assetID[:], testAssetID)

	metaHash := tapsdk.Hash{}
	copy(metaHash[:], testAssetID)

	tests := []struct {
		name     string
		req      *tapsdk.FetchAssetMetaRequest
		validate func(*testing.T, *taprpc.FetchAssetMetaRequest)
	}{
		{
			name: "fetch by asset ref",
			req: &tapsdk.FetchAssetMetaRequest{
				AssetRef: func() *tapsdk.AssetRef {
					ref := tapsdk.AssetRefFromAssetID(assetID)
					return &ref
				}(),
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.FetchAssetMetaRequest) {

				require.Equal(
					t, testAssetID,
					rpcReq.GetAssetId(),
				)
			},
		},
		{
			name: "fetch by meta hash",
			req: &tapsdk.FetchAssetMetaRequest{
				MetaHash: &metaHash,
			},
			validate: func(t *testing.T,
				rpcReq *taprpc.FetchAssetMetaRequest) {

				require.Equal(
					t, testAssetID,
					rpcReq.GetMetaHash(),
				)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalFetchAssetMetaRequest(tc.req)
			require.NoError(t, err)
			tc.validate(t, rpcReq)
		})
	}
}

func TestUnmarshalAssetMeta(t *testing.T) {
	tests := []struct {
		name    string
		resp    *taprpc.FetchAssetMetaResponse
		wantErr string
	}{
		{
			name:    "nil response",
			resp:    nil,
			wantErr: "nil asset meta response",
		},
		{
			name: "valid meta",
			resp: &taprpc.FetchAssetMetaResponse{
				Data:                  []byte("test data"),
				Type:                  taprpc.AssetMetaType_META_TYPE_JSON,
				MetaHash:              testAssetID,
				DecimalDisplay:        8,
				UniverseCommitments:   true,
				CanonicalUniverseUrls: []string{"https://uni.example.com"},
				DelegationKey:         testPubKey,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := unmarshalFetchAssetMetaResponse(tc.resp)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(
				t, tapsdk.AssetMetaTypeJSON,
				result.Type,
			)
			require.Equal(
				t, uint32(8), result.DecimalDisplay,
			)
			require.True(t, result.UniverseCommitments)
			require.Len(
				t, result.CanonicalUniverseURLs, 1,
			)
		})
	}
}
