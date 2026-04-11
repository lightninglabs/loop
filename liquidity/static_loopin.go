package liquidity

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing/route"
)

// StaticLoopInInfo contains the persisted data that liquidity needs for budget
// accounting and peer traffic tracking.
type StaticLoopInInfo struct {
	// Label identifies whether the swap belongs to autoloop.
	Label string

	// QuotedSwapFee is the quoted server fee for the swap.
	QuotedSwapFee btcutil.Amount

	// HtlcTxFeeRate is the stored HTLC transaction fee rate for the swap's
	// timeout path. This is only known once the static loop-in has been
	// initiated and the server has proposed concrete HTLC transactions.
	HtlcTxFeeRate chainfee.SatPerKWeight

	// LastHop identifies the target peer when the swap is peer-restricted.
	LastHop *route.Vertex

	// LastUpdateTime is the timestamp of the latest persisted state update.
	LastUpdateTime time.Time

	// Pending indicates whether the swap is still in flight and therefore
	// needs worst-case fee reservation in the current budget window.
	Pending bool

	// Failed indicates whether the swap reached a terminal failure state.
	// Liquidity uses this to apply conservative fee accounting and recent
	// failure backoff for the peer.
	Failed bool

	// BlocksLoopIn indicates whether the swap should currently block new
	// loop-in suggestions for its peer. Static swaps stop blocking once the
	// off-chain payment has been received.
	BlocksLoopIn bool

	// NumDeposits is the number of deposits locked into the swap.
	NumDeposits int

	// HasChange indicates whether the swap selected less than the total
	// value of its deposits and therefore produced change.
	HasChange bool
}

// staticLoopInWorstCaseFees returns the larger of the cooperative success fee
// and the timeout-path fee for a static loop-in.
func staticLoopInWorstCaseFees(numDeposits int, hasChange bool,
	swapFee btcutil.Amount, htlcFeeRate,
	timeoutSweepFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	successFee := swapFee

	timeoutFee := staticLoopInOnchainFee(
		numDeposits, hasChange, htlcFeeRate, timeoutSweepFeeRate,
	)

	return max(timeoutFee, successFee)
}

// staticLoopInOnchainFee estimates the fee for the server-published HTLC
// transaction and client sweep transaction.
func staticLoopInOnchainFee(numDeposits int, hasChange bool, htlcFeeRate,
	timeoutSweepFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	htlcFeeRate = staticLoopInHtlcFeeRate(
		htlcFeeRate, timeoutSweepFeeRate,
	)

	htlcFee := htlcFeeRate.FeeForWeight(
		staticLoopInHtlcWeight(numDeposits, hasChange),
	)

	sweepFee := loopInSweepFee(timeoutSweepFeeRate)

	return htlcFee + sweepFee
}

// staticLoopInHtlcFeeRate returns the best HTLC fee rate known to the planner.
// Pending static loop-ins do not persist their concrete HTLC fee rate until the
// server returns the HTLC package, so liquidity has to reuse the same
// conservative fallback it already uses for dry-run filtering when the stored
// rate is still zero.
func staticLoopInHtlcFeeRate(htlcFeeRate,
	timeoutSweepFeeRate chainfee.SatPerKWeight) chainfee.SatPerKWeight {

	if htlcFeeRate == 0 {
		return timeoutSweepFeeRate
	}

	return htlcFeeRate
}

// staticLoopInHtlcWeight returns the HTLC transaction weight for a static loop
// in with the given number of deposits.
func staticLoopInHtlcWeight(numDeposits int,
	hasChange bool) lntypes.WeightUnit {

	var estimator input.TxWeightEstimator

	for range numDeposits {
		estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	}

	estimator.AddP2WSHOutput()

	if hasChange {
		estimator.AddP2TROutput()
	}

	return estimator.Weight()
}
