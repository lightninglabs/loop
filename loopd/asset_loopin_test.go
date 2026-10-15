package loopd

import (
	"testing"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/stretchr/testify/require"
)

// TestAssetLoopInStatus covers the shared marshaller used by SwapInfo,
// ListSwaps and Monitor, including an asset-only filter after loading terms.
func TestAssetLoopInStatus(t *testing.T) {
	for _, group := range []bool{false, true} {
		info := &loopdb.LoopInAssetSwap{
			AssetId: []byte{1}, MinAssetAmount: 4964, AssetAmount: 4964,
			RateCoefficient: "1000000", QuoteExpiry: 1234,
		}
		if group {
			info.AssetId = nil
			info.GroupKey = []byte{2}
		}
		s := &loop.SwapInfo{
			SwapType: swap.TypeIn, HtlcAddressP2TR: testnetAddr,
			AssetLoopInInfo: info,
		}
		server := &swapClientServer{}
		rpc, err := server.marshallSwap(t.Context(), s)
		require.NoError(t, err)
		require.Equal(t, info.AssetId, rpc.AssetLoopInInfo.AssetId)
		require.Equal(t, info.GroupKey, rpc.AssetLoopInInfo.GroupKey)
		require.Equal(t, info.MinAssetAmount, rpc.AssetLoopInInfo.MinAssetAmount)
		require.Equal(t, info.AssetAmount, rpc.AssetLoopInInfo.AssetAmount)
		require.Equal(t, info.RateCoefficient, rpc.AssetLoopInInfo.AssetRate.Coefficient)
		filter := &looprpc.ListSwapsFilter{AssetSwapOnly: true}
		require.True(t, filterSwap(s, filter))
		s.AssetLoopInInfo = nil
		require.False(t, filterSwap(s, filter))
	}
}
