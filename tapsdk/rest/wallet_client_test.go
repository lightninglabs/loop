package rest

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	restTestPubKey = []byte{
		0x02,
		0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac,
		0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
		0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9,
		0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98,
	}

	restTestAssetID = []byte{
		0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8,
		0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf, 0xb0,
		0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8,
		0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf, 0xc0,
	}
)

const restZeroOutpoint = "00000000000000000000000000000000" +
	"00000000000000000000000000000000:1"

func TestAssetRecordMatchesRef(t *testing.T) {
	var assetID tapsdk.AssetID
	copy(assetID[:], restTestAssetID)
	itemRef := tapsdk.AssetRefFromAssetID(assetID)

	var groupKey tapsdk.PubKey
	copy(groupKey[:], restTestPubKey)
	collectionRef := tapsdk.AssetRefFromGroupKey(groupKey)

	var otherID tapsdk.AssetID
	copy(otherID[:], restTestAssetID)
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

func TestListAssetRecordsUsesFilterQuery(t *testing.T) {
	var groupKey tapsdk.PubKey
	copy(groupKey[:], restTestPubKey)
	ref := tapsdk.AssetRefFromGroupKey(groupKey)

	var anchorTxid [32]byte
	copy(anchorTxid[:], restTestAssetID)
	anchor := tapsdk.Outpoint{
		Txid:  anchorTxid,
		Index: 7,
	}

	explicitType := tapsdk.ScriptKeyTypeBurn

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {

		query := r.URL.Query()
		assert.Equal(t, "/v1/taproot-assets/assets", r.URL.Path)
		assert.Equal(t, "true", query.Get("with_witness"))
		assert.Equal(t, "true", query.Get("include_spent"))
		assert.Equal(t, "true", query.Get("include_leased"))
		assert.Equal(
			t, "true", query.Get("include_unconfirmed_mints"),
		)
		assert.Equal(t, "7", query.Get("min_amount"))
		assert.Equal(t, "11", query.Get("max_amount"))
		assert.Equal(
			t,
			base64.URLEncoding.EncodeToString(restTestPubKey),
			query.Get("group_key"),
		)
		assert.Equal(
			t,
			base64.URLEncoding.EncodeToString(restTestAssetID),
			query.Get("anchor_outpoint.txid"),
		)
		assert.Equal(
			t, "7", query.Get("anchor_outpoint.output_index"),
		)
		assert.Equal(
			t, "SCRIPT_KEY_BURN",
			query.Get("script_key_type.explicit_type"),
		)

		_, err := fmt.Fprint(w, `{"assets":[]}`)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	client := newWalletClient(&transport{
		baseURL:   srv.URL,
		client:    srv.Client(),
		timeout:   time.Second,
		macaroons: macaroon.Pouch{},
	})

	assets, err := client.ListAssetRecords(
		context.Background(), &tapsdk.ListAssetsRequest{
			WithWitness:             true,
			IncludeSpent:            true,
			IncludeLeased:           true,
			IncludeUnconfirmedMints: true,
			MinAmount:               7,
			MaxAmount:               11,
			AssetRef:                &ref,
			AnchorOutpoint:          &anchor,
			ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{
				ExplicitType: &explicitType,
			},
		},
	)
	require.NoError(t, err)
	require.Empty(t, assets)
}

func TestListAssetRecordsQueryParams(t *testing.T) {
	var assetID tapsdk.AssetID
	copy(assetID[:], restTestAssetID)
	assetIDRef := tapsdk.AssetRefFromAssetID(assetID)

	var groupKey tapsdk.PubKey
	copy(groupKey[:], restTestPubKey)
	groupRef := tapsdk.AssetRefFromGroupKey(groupKey)

	invalidType := tapsdk.ScriptKeyType(99)

	tests := []struct {
		name     string
		req      *tapsdk.ListAssetsRequest
		wantRef  *tapsdk.AssetRef
		wantErr  string
		validate func(*testing.T, map[string][]string)
	}{
		{
			name: "nil request",
			req:  nil,
			validate: func(t *testing.T, values map[string][]string) {
				require.Empty(t, values)
			},
		},
		{
			name: "zero amount bounds omitted",
			req:  &tapsdk.ListAssetsRequest{},
			validate: func(t *testing.T, values map[string][]string) {
				require.NotContains(t, values, "min_amount")
				require.NotContains(t, values, "max_amount")
			},
		},
		{
			name:    "asset ID ref uses local filter only",
			req:     &tapsdk.ListAssetsRequest{AssetRef: &assetIDRef},
			wantRef: &assetIDRef,
			validate: func(t *testing.T, values map[string][]string) {
				require.NotContains(t, values, "group_key")
			},
		},
		{
			name:    "group ref uses server filter",
			req:     &tapsdk.ListAssetsRequest{AssetRef: &groupRef},
			wantRef: &groupRef,
			validate: func(t *testing.T, values map[string][]string) {
				require.Equal(
					t,
					base64.URLEncoding.EncodeToString(
						restTestPubKey,
					),
					values["group_key"][0],
				)
			},
		},
		{
			name: "all script key types",
			req: &tapsdk.ListAssetsRequest{
				ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{
					AllTypes: true,
				},
			},
			validate: func(t *testing.T, values map[string][]string) {
				require.Equal(
					t, "true",
					values["script_key_type.all_types"][0],
				)
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
			params, ref, err := listAssetRecordsQueryParams(tc.req)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantRef, ref)
			tc.validate(t, params)
		})
	}
}

func TestMarshalScriptKeyType(t *testing.T) {
	tests := []struct {
		name          string
		scriptKeyType tapsdk.ScriptKeyType
		want          string
	}{
		{
			name:          "unknown",
			scriptKeyType: tapsdk.ScriptKeyTypeUnknown,
			want:          "SCRIPT_KEY_UNKNOWN",
		},
		{
			name:          "bip86",
			scriptKeyType: tapsdk.ScriptKeyTypeBIP86,
			want:          "SCRIPT_KEY_BIP86",
		},
		{
			name:          "script path external",
			scriptKeyType: tapsdk.ScriptKeyTypeScriptPathExternal,
			want:          "SCRIPT_KEY_SCRIPT_PATH_EXTERNAL",
		},
		{
			name:          "burn",
			scriptKeyType: tapsdk.ScriptKeyTypeBurn,
			want:          "SCRIPT_KEY_BURN",
		},
		{
			name:          "tombstone",
			scriptKeyType: tapsdk.ScriptKeyTypeTombstone,
			want:          "SCRIPT_KEY_TOMBSTONE",
		},
		{
			name:          "channel",
			scriptKeyType: tapsdk.ScriptKeyTypeChannel,
			want:          "SCRIPT_KEY_CHANNEL",
		},
		{
			name:          "unique pedersen",
			scriptKeyType: tapsdk.ScriptKeyTypeUniquePedersen,
			want:          "SCRIPT_KEY_UNIQUE_PEDERSEN",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(
				t, tc.want,
				marshalScriptKeyType(tc.scriptKeyType),
			)
		})
	}
}

func TestBurnAssetRequestBody(t *testing.T) {
	var assetID tapsdk.AssetID
	copy(assetID[:], restTestAssetID)

	var groupKey tapsdk.PubKey
	copy(groupKey[:], restTestPubKey)

	tests := []struct {
		name     string
		req      *tapsdk.BurnAssetRequest
		validate func(*testing.T, map[string]any)
	}{
		{
			name: "asset ID ref uses asset specifier",
			req: &tapsdk.BurnAssetRequest{
				AssetRef:         tapsdk.AssetRefFromAssetID(assetID),
				AmountToBurn:     100,
				ConfirmationText: "assets will be destroyed",
				Note:             "test",
			},
			validate: func(t *testing.T, body map[string]any) {
				specifier, ok := body["asset_specifier"].(map[string]string)
				require.True(t, ok)
				require.Equal(
					t, hex.EncodeToString(restTestAssetID),
					specifier["asset_id_str"],
				)
				require.NotContains(t, body, "asset_id_str")
				require.Equal(t, "100", body["amount_to_burn"])
				require.Equal(t, "test", body["note"])
			},
		},
		{
			name: "group key ref uses asset specifier",
			req: &tapsdk.BurnAssetRequest{
				AssetRef:     tapsdk.AssetRefFromGroupKey(groupKey),
				AmountToBurn: 200,
			},
			validate: func(t *testing.T, body map[string]any) {
				specifier, ok := body["asset_specifier"].(map[string]string)
				require.True(t, ok)
				require.Equal(
					t, hex.EncodeToString(restTestPubKey),
					specifier["group_key_str"],
				)
				require.NotContains(t, body, "asset_id_str")
				require.Equal(t, "200", body["amount_to_burn"])
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := burnAssetRequestBody(tc.req)
			require.NoError(t, err)

			tc.validate(t, body)
		})
	}
}

func TestListBurnsUsesTweakedGroupKeyQuery(t *testing.T) {
	var groupKey tapsdk.PubKey
	copy(groupKey[:], restTestPubKey)
	ref := tapsdk.AssetRefFromGroupKey(groupKey)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {

		assert.Equal(t, "/v1/taproot-assets/burns", r.URL.Path)
		assert.Empty(t, r.URL.Query().Get("group_key"))
		assert.Equal(
			t, base64.URLEncoding.EncodeToString(restTestPubKey),
			r.URL.Query().Get("tweaked_group_key"),
		)

		_, err := fmt.Fprint(w, `{"burns":[]}`)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	client := newWalletClient(&transport{
		baseURL:   srv.URL,
		client:    srv.Client(),
		timeout:   time.Second,
		macaroons: macaroon.Pouch{},
	})

	burns, err := client.ListBurns(
		context.Background(), &tapsdk.ListBurnsRequest{
			AssetRef: &ref,
		},
	)
	require.NoError(t, err)
	require.Empty(t, burns)
}

func TestUnmarshalAssetTransferGroupKey(t *testing.T) {
	pubKeyHex := hex.EncodeToString(restTestPubKey)
	assetIDHex := hex.EncodeToString(restTestAssetID)

	transfer, err := unmarshalAssetTransfer(&jsonAssetTransfer{
		Inputs: []*jsonTransferInput{{
			AnchorPoint: restZeroOutpoint,
			AssetID:     assetIDHex,
			AssetType:   assetTypeNormalJSON,
			ScriptKey:   pubKeyHex,
			Amount:      "100",
			GroupKey:    pubKeyHex,
		}},
		Outputs: []*jsonTransferOutput{{
			Amount:    "60",
			AssetID:   assetIDHex,
			AssetType: assetTypeNormalJSON,
			ScriptKey: pubKeyHex,
			Anchor: &jsonAnchorInfo{
				Outpoint: restZeroOutpoint,
				Value:    "330",
			},
			GroupKey: pubKeyHex,
		}},
	})
	require.NoError(t, err)
	require.Len(t, transfer.Inputs, 1)
	require.Len(t, transfer.Outputs, 1)

	require.NotNil(t, transfer.Inputs[0].GroupKey)
	require.Equal(t, restTestPubKey, transfer.Inputs[0].GroupKey[:])
	require.Equal(t,
		tapsdk.AssetTypeNormal, transfer.Inputs[0].AssetType)
	require.NotNil(t, transfer.Outputs[0].GroupKey)
	require.Equal(t, restTestPubKey, transfer.Outputs[0].GroupKey[:])
	require.Equal(t,
		tapsdk.AssetTypeNormal, transfer.Outputs[0].AssetType)
}

func TestUnmarshalAssetTransferCollectionItem(t *testing.T) {
	pubKeyHex := hex.EncodeToString(restTestPubKey)
	assetIDHex := hex.EncodeToString(restTestAssetID)

	transfer, err := unmarshalAssetTransfer(&jsonAssetTransfer{
		Inputs: []*jsonTransferInput{{
			AnchorPoint: restZeroOutpoint,
			AssetID:     assetIDHex,
			AssetType:   assetTypeCollectibleJSON,
			ScriptKey:   pubKeyHex,
			Amount:      "1",
			GroupKey:    pubKeyHex,
		}},
		Outputs: []*jsonTransferOutput{{
			Amount:    "1",
			AssetID:   assetIDHex,
			AssetType: assetTypeCollectibleJSON,
			ScriptKey: pubKeyHex,
			Anchor: &jsonAnchorInfo{
				Outpoint: restZeroOutpoint,
				Value:    "330",
			},
			GroupKey: pubKeyHex,
		}},
	})
	require.NoError(t, err)

	var assetID tapsdk.AssetID
	copy(assetID[:], restTestAssetID)
	wantRef := tapsdk.AssetRefFromAssetID(assetID)

	highLevel := tapsdk.NewTransfer(transfer)
	require.True(t, highLevel.Inputs[0].AssetRef.Equivalent(wantRef))
	require.True(t, highLevel.Outputs[0].AssetRef.Equivalent(wantRef))
	require.Equal(t,
		tapsdk.AssetTypeCollectible, highLevel.Inputs[0].Type)
}

func TestUnmarshalAddrCollectionItemUsesAssetID(t *testing.T) {
	pubKeyHex := hex.EncodeToString(restTestPubKey)
	assetIDHex := hex.EncodeToString(restTestAssetID)

	addr, err := unmarshalAddr(&jsonAddr{
		Encoded:          "tap1collection",
		AssetID:          assetIDHex,
		AssetType:        assetTypeCollectibleJSON,
		Amount:           "1",
		GroupKey:         pubKeyHex,
		ScriptKey:        pubKeyHex,
		InternalKey:      pubKeyHex,
		TaprootOutputKey: hex.EncodeToString(restTestAssetID),
		AssetVersion:     testAssetVersionV1,
		AddressVersion:   testAddrVersionV2,
	})
	require.NoError(t, err)

	var assetID tapsdk.AssetID
	copy(assetID[:], restTestAssetID)
	require.True(t,
		addr.AssetRef.Equivalent(tapsdk.AssetRefFromAssetID(assetID)))
}

func TestUnmarshalAddrCollectionAddressUsesGroupKey(t *testing.T) {
	pubKeyHex := hex.EncodeToString(restTestPubKey)

	addr, err := unmarshalAddr(&jsonAddr{
		Encoded:          "tap1collection",
		AssetID:          hex.EncodeToString(make([]byte, 32)),
		AssetType:        assetTypeCollectibleJSON,
		Amount:           "1",
		GroupKey:         pubKeyHex,
		ScriptKey:        pubKeyHex,
		InternalKey:      pubKeyHex,
		TaprootOutputKey: hex.EncodeToString(restTestAssetID),
		AssetVersion:     testAssetVersionV1,
		AddressVersion:   testAddrVersionV2,
	})
	require.NoError(t, err)

	var groupKey tapsdk.PubKey
	copy(groupKey[:], restTestPubKey)
	require.True(t,
		addr.AssetRef.Equivalent(tapsdk.AssetRefFromGroupKey(groupKey)))
}

func TestUnmarshalAddrRejectsMissingAssetRef(t *testing.T) {
	pubKeyHex := hex.EncodeToString(restTestPubKey)

	_, err := unmarshalAddr(&jsonAddr{
		Encoded:          "tap1missingref",
		AssetType:        assetTypeNormalJSON,
		Amount:           "1",
		ScriptKey:        pubKeyHex,
		InternalKey:      pubKeyHex,
		TaprootOutputKey: hex.EncodeToString(restTestAssetID),
		AssetVersion:     testAssetVersionV1,
		AddressVersion:   testAddrVersionV2,
	})
	require.ErrorContains(t, err, "missing asset ID or group key")
}

func TestUnmarshalAddrRejectsZeroAssetIDWithoutGroupKey(t *testing.T) {
	pubKeyHex := hex.EncodeToString(restTestPubKey)

	_, err := unmarshalAddr(&jsonAddr{
		Encoded:          "tap1zeroasset",
		AssetID:          hex.EncodeToString(make([]byte, 32)),
		AssetType:        assetTypeCollectibleJSON,
		Amount:           "1",
		ScriptKey:        pubKeyHex,
		InternalKey:      pubKeyHex,
		TaprootOutputKey: hex.EncodeToString(restTestAssetID),
		AssetVersion:     testAssetVersionV1,
		AddressVersion:   testAddrVersionV2,
	})
	require.ErrorContains(t, err, "zero with no group key")
}
