package liquidity

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Compile time assertion that loop in suggestions satisfy our interface.
var _ swapSuggestion = (*loopInSwapSuggestion)(nil)

type loopInSwapSuggestion struct {
	loop.LoopInRequest
}

// amount returns the amount of the swap suggestion.
func (l *loopInSwapSuggestion) amount() btcutil.Amount {
	return l.LoopInRequest.Amount
}

// fees returns the highest fees that we could pay for the swap suggestion.
func (l *loopInSwapSuggestion) fees() btcutil.Amount {
	return worstCaseInFees(
		l.LoopInRequest.MaxMinerFee, l.LoopInRequest.MaxSwapFee,
		defaultLoopInSweepFee,
	)
}

// channels returns no channels for loop in swap suggestions because we do not
// restrict loop in swaps by channel id.
func (l *loopInSwapSuggestion) channels() []lnwire.ShortChannelID {
	return nil
}

// peers returns the peer that a loop in swap is restricted to, if it is set.
func (l *loopInSwapSuggestion) peers(_ map[uint64]route.Vertex) []route.Vertex {
	if l.LoopInRequest.LastHop == nil {
		return nil
	}

	return []route.Vertex{
		*l.LoopInRequest.LastHop,
	}
}

// worstCaseInFees returns the largest possible fees for a loop in swap.
func worstCaseInFees(maxMinerFee, swapFee btcutil.Amount,
	sweepEst chainfee.SatPerKWeight) btcutil.Amount {

	failureFee := maxMinerFee + loopInSweepFee(sweepEst)
	successFee := maxMinerFee + swapFee

	if failureFee > successFee {
		return failureFee
	}

	return successFee
}

// loopInSweepFee provides an estimated fee for our sweep transaction, based
// on the fee rate provided. We can calculate our fees for htlcv2 and p2wkh
// timeout addresses because automated loop ins will be handled entirely by the
// client, so we know what types will be used.
func loopInSweepFee(fee chainfee.SatPerKWeight) btcutil.Amount {
	var estimator input.TxWeightEstimator

	// We sweep loop in swaps to wpkh addresses provided by lnd.
	estimator.AddP2WKHOutput()

	// Create a htlcv2, which is what all autoloops will use, so that we
	// can get our maximum timeout witness size.
	htlc := swap.HtlcScriptV2{}
	maxSize := htlc.MaxTimeoutWitnessSize()

	estimator.AddWitnessInput(maxSize)
	weight := int64(estimator.Weight())

	return fee.FeeForWeight(weight)
}
