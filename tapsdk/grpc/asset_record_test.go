package grpc

import (
	"strings"
	"testing"

	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalAssetAnchorOutpoint(t *testing.T) {
	hash := strings.Repeat("0123456789abcdef", 4)
	for _, test := range []struct {
		name   string
		anchor *taprpc.AnchorInfo
		valid  bool
	}{
		{"missing", nil, true},
		{"unconfirmed", &taprpc.AnchorInfo{}, true},
		{"canonical", &taprpc.AnchorInfo{
			AnchorOutpoint: hash + ":7"}, true},
		{"uppercase", &taprpc.AnchorInfo{
			AnchorOutpoint: strings.ToUpper(hash) + ":7"}, false},
		{"short hash", &taprpc.AnchorInfo{
			AnchorOutpoint: "1:7"}, false},
		{"zero padded index", &taprpc.AnchorInfo{
			AnchorOutpoint: hash + ":07"}, false},
		{"signed index", &taprpc.AnchorInfo{
			AnchorOutpoint: hash + ":+7"}, false},
		{"overflow index", &taprpc.AnchorInfo{
			AnchorOutpoint: hash + ":4294967296"}, false},
		{"malformed", &taprpc.AnchorInfo{AnchorOutpoint: hash}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := unmarshalAsset(&taprpc.Asset{
				AssetGenesis: &taprpc.GenesisInfo{
					GenesisPoint: zeroGenesisPoint,
					AssetId:      testAssetID,
				},
				ScriptKey: testPubKey, ChainAnchor: test.anchor,
			})
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
