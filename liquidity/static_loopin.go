package liquidity

import (
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrNoStaticLoopInCandidate is returned when the static-address side
	// is unable to build a full-deposit, no-change candidate for an
	// autoloop target. This sentinel lets the planner surface a structured
	// reason without silently falling back to wallet-funded loop-ins.
	ErrNoStaticLoopInCandidate = errors.New("no static loop-in candidate")
)

// Compile-time assertion that static loop-in suggestions satisfy the shared
// swap suggestion interface.
var _ swapSuggestion = (*staticLoopInSwapSuggestion)(nil)

// PreparedStaticLoopIn contains the dry-run data that liquidity needs in order
// to represent and account for a static loop-in suggestion.
type PreparedStaticLoopIn struct {
	// Request is the fully specified loop-in request that should be used if
	// the suggestion is later dispatched.
	Request loop.StaticAddressLoopInRequest

	// NumDeposits is the number of deposits selected for the request. We
	// keep this separate so that fee estimation does not need to inspect
	// any static-address-specific types.
	NumDeposits int

	// HasChange indicates whether the selected deposits would create
	// change. The initial autoloop implementation always keeps this false,
	// but the flag is included so that future partial-selection modes can
	// reuse the same accounting path safely.
	HasChange bool
}

// StaticLoopInDispatchResult contains the values that autoloop logs after a
// static loop-in is dispatched.
type StaticLoopInDispatchResult struct {
	// SwapHash is the static loop-in swap identifier.
	SwapHash lntypes.Hash
}

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

// staticLoopInSwapSuggestion is the suggested representation of a static loop
// in request.
type staticLoopInSwapSuggestion struct {
	// request is the request that will be dispatched if autoloop executes
	// the suggestion.
	request loop.StaticAddressLoopInRequest

	// numDeposits is the number of deposits consumed by the swap. This
	// feeds the conservative HTLC fee estimate used for budget filtering.
	numDeposits int

	// hasChange indicates whether the suggestion would create change.
	hasChange bool
}

// staticLoopInCandidate is the pre-preparation representation of a static
// loop-in rule match. It carries the peer target and desired amount so the
// planner can sort candidates before allocating concrete deposits.
type staticLoopInCandidate struct {
	// peer is the target peer for the loop-in.
	peer route.Vertex

	// minAmount is the minimum swap size that the eventual full-deposit
	// selection must still satisfy after any allowed undershoot.
	minAmount btcutil.Amount

	// amountHint is the maximum amount that the planner should try to cover
	// with full-deposit static selection.
	amountHint btcutil.Amount

	// channelSet carries the peer aggregate's channels so disqualification
	// reasons can still be attached consistently during later filtering.
	channelSet []lnwire.ShortChannelID
}

// amount returns the desired amount for the candidate.
func (s *staticLoopInCandidate) amount() btcutil.Amount {
	return s.amountHint
}

// channels returns the channels that belong to the target peer aggregate.
func (s *staticLoopInCandidate) channels() []lnwire.ShortChannelID {
	return s.channelSet
}

// peers returns the single target peer for the candidate.
func (s *staticLoopInCandidate) peers(
	_ map[uint64]route.Vertex) []route.Vertex {

	return []route.Vertex{s.peer}
}

// amount returns the selected swap amount for the suggestion.
func (s *staticLoopInSwapSuggestion) amount() btcutil.Amount {
	return s.request.SelectedAmount
}

// fees returns the worst-case fee estimate for a static loop-in suggestion.
func (s *staticLoopInSwapSuggestion) fees() btcutil.Amount {
	// The actual HTLC fee rate is only known once the server returns the
	// signed HTLC packages during initiation. For dry-run planning we use
	// the same conservative fee-rate constant that loop-in sweep budgeting
	// already uses so that static suggestions do not undercount timeout
	// risk.
	return staticLoopInWorstCaseFees(
		s.numDeposits, s.hasChange, s.request.MaxSwapFee,
		defaultLoopInSweepFee, defaultLoopInSweepFee,
	)
}

// channels returns no channels because loop-in rules are peer-scoped.
func (s *staticLoopInSwapSuggestion) channels() []lnwire.ShortChannelID {
	return nil
}

// peers returns the peer that the static loop-in suggestion targets.
func (s *staticLoopInSwapSuggestion) peers(
	_ map[uint64]route.Vertex) []route.Vertex {

	if s.request.LastHop == nil {
		return nil
	}

	return []route.Vertex{*s.request.LastHop}
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

// staticLoopInFeeLimit checks a static loop-in candidate against the active
// fee policy using the static swap's own worst-case fee model instead of the
// legacy wallet-funded loop-in assumptions.
func staticLoopInFeeLimit(feeLimit FeeLimit, amount, swapFee btcutil.Amount,
	numDeposits int, hasChange bool) error {

	switch limit := feeLimit.(type) {
	case *FeeCategoryLimit:
		maxServerFee := ppmToSat(amount, limit.MaximumSwapFeePPM)
		if swapFee > maxServerFee {
			return newReasonError(ReasonSwapFee)
		}

		// We do not know the final HTLC fee rate until the server
		// returns concrete HTLC packages during initiation, so the
		// planner has to reuse the same conservative default that
		// dry-run budget filtering already uses.
		onchainFees := staticLoopInOnchainFee(
			numDeposits, hasChange, defaultLoopInSweepFee,
			defaultLoopInSweepFee,
		)

		if onchainFees > limit.MaximumMinerFee {
			return newReasonError(ReasonMinerFee)
		}

		return nil

	case *FeePortion:
		totalFeeSpend := ppmToSat(amount, limit.PartsPerMillion)
		if swapFee > totalFeeSpend {
			return newReasonError(ReasonSwapFee)
		}

		fees := staticLoopInWorstCaseFees(
			numDeposits, hasChange, swapFee, defaultLoopInSweepFee,
			defaultLoopInSweepFee,
		)
		if fees > totalFeeSpend {
			return newReasonError(ReasonFeePPMInsufficient)
		}

		return nil

	default:
		return fmt.Errorf("unknown fee limit: %T", feeLimit)
	}
}
