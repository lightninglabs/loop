package loopd

import (
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
)

// marshalAssetLoopInInfo exposes the same persisted terms in all status APIs.
func marshalAssetLoopInInfo(info *loopdb.LoopInAssetSwap) *looprpc.AssetLoopInInfo {
	if info == nil {
		return nil
	}
	return &looprpc.AssetLoopInInfo{
		AssetId:        info.AssetId,
		GroupKey:       info.GroupKey,
		AssetEdgeNode:  info.AssetEdgeNode,
		MinAssetAmount: info.MinAssetAmount,
		AssetAmount:    info.AssetAmount,
		AssetRate: &looprpc.FixedPoint{
			Coefficient: info.RateCoefficient,
			Scale:       info.RateScale,
		},
		QuoteExpiry: info.QuoteExpiry,
	}
}

// unmarshalAssetLoopInQuote translates the optional receiving selector.
func unmarshalAssetLoopInQuote(
	info *looprpc.AssetLoopInRequest) *loop.AssetLoopInQuoteRequest {

	if info == nil {
		return nil
	}
	return &loop.AssetLoopInQuoteRequest{
		AssetId:        info.AssetId,
		GroupKey:       info.GroupKey,
		AssetEdgeNode:  info.AssetEdgeNode,
		MinAssetAmount: info.MinAssetAmount,
	}
}
