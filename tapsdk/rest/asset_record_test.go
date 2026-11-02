package rest

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnmarshalAssetAnchorOutpoint(t *testing.T) {
	hash := strings.Repeat("0123456789abcdef", 4)
	for _, test := range []struct {
		name   string
		anchor *jsonChainAnchor
		valid  bool
	}{
		{"missing", nil, true},
		{"unconfirmed", &jsonChainAnchor{}, true},
		{"canonical", &jsonChainAnchor{
			AnchorOutpoint: hash + ":7"}, true},
		{"uppercase", &jsonChainAnchor{
			AnchorOutpoint: strings.ToUpper(hash) + ":7"}, false},
		{"short hash", &jsonChainAnchor{
			AnchorOutpoint: "1:7"}, false},
		{"zero padded index", &jsonChainAnchor{
			AnchorOutpoint: hash + ":07"}, false},
		{"signed index", &jsonChainAnchor{
			AnchorOutpoint: hash + ":+7"}, false},
		{"overflow index", &jsonChainAnchor{
			AnchorOutpoint: hash + ":4294967296"}, false},
		{"malformed", &jsonChainAnchor{AnchorOutpoint: hash}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Round-trip the public JSON name as well as the typed
			// mapper.
			raw, err := json.Marshal(map[string]any{
				"asset_genesis": &jsonGenesisInfo{
					GenesisPoint: hash + ":1",
					AssetID: hex.EncodeToString(
						restTestAssetID,
					),
					AssetType: assetTypeNormalJSON,
				},
				"script_key": hex.EncodeToString(
					restTestPubKey,
				),
				"chain_anchor": test.anchor,
			})
			require.NoError(t, err)
			var asset jsonAsset
			require.NoError(t, json.Unmarshal(raw, &asset))
			record, err := unmarshalAsset(&asset)
			if !test.valid {
				require.ErrorContains(
					t,
					err,
					"canonical anchor outpoint",
				)
				require.Nil(t, record)
				return
			}
			require.NoError(t, err)
			if test.anchor == nil ||
				test.anchor.AnchorOutpoint == "" {

				require.Nil(t, record.AnchorOutpoint)
				return
			}
			require.NotNil(t, record.AnchorOutpoint)
			require.Equal(
				t,
				hash+":7",
				record.AnchorOutpoint.String(),
			)
			require.EqualValues(t, 7, record.AnchorOutpoint.Index)
			require.EqualValues(
				t,
				0xef,
				record.AnchorOutpoint.Txid[0],
			)
			require.EqualValues(
				t,
				0x01,
				record.AnchorOutpoint.Txid[31],
			)
		})
	}
}
