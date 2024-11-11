package loop

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// sweeper provides fee, fee rate and weight by confTarget.
type sweeper interface {
	// GetSweepFeeDetails calculates the required tx fee to spend to
	// destAddr. It takes a function that is expected to add the weight of
	// the input to the weight estimator. It also takes a label used for
	// logging. It returns also the fee rate and transaction weight.
	GetSweepFeeDetails(ctx context.Context,
		addInputEstimate func(*input.TxWeightEstimator) error,
		destAddr btcutil.Address, sweepConfTarget int32, label string) (
		btcutil.Amount, chainfee.SatPerKWeight, lntypes.WeightUnit,
		error)
}

// loopOutFetcher provides the loop out swap with the given hash.
type loopOutFetcher interface {
	// FetchLoopOutSwap returns the loop out swap with the given hash.
	FetchLoopOutSwap(ctx context.Context,
		hash lntypes.Hash) (*loopdb.LoopOut, error)
}

// heightGetter returns current height known to the swap server.
type heightGetter func() int32

// loopOutSweepFeerateProvider provides sweepbatcher with the info about swap's
// current feerate for loop-out sweep.
type loopOutSweepFeerateProvider struct {
	// sweeper provides fee, fee rate and weight by confTarget.
	sweeper sweeper

	// loopOutFetcher loads LoopOut from DB by swap hash.
	loopOutFetcher loopOutFetcher

	// chainParams are the chain parameters of the chain that is used by
	// swaps.
	chainParams *chaincfg.Params

	// getHeight returns current height known to the swap server.
	getHeight heightGetter
}

// newLoopOutSweepFeerateProvider builds and returns new instance of
// loopOutSweepFeerateProvider.
func newLoopOutSweepFeerateProvider(sweeper sweeper,
	loopOutFetcher loopOutFetcher, chainParams *chaincfg.Params,
	getHeight heightGetter) *loopOutSweepFeerateProvider {

	return &loopOutSweepFeerateProvider{
		sweeper:        sweeper,
		loopOutFetcher: loopOutFetcher,
		chainParams:    chainParams,
		getHeight:      getHeight,
	}
}

// GetMinFeeRate returns minimum required feerate for a sweep by swap hash.
func (p *loopOutSweepFeerateProvider) GetMinFeeRate(ctx context.Context,
	swapHash lntypes.Hash) (chainfee.SatPerKWeight, error) {

	_, feeRate, err := p.GetConfTargetAndFeeRate(ctx, swapHash)

	return feeRate, err
}

// GetConfTargetAndFeeRate returns conf target and minimum required feerate
// for a sweep by swap hash.
func (p *loopOutSweepFeerateProvider) GetConfTargetAndFeeRate(
	ctx context.Context, swapHash lntypes.Hash) (int32,
	chainfee.SatPerKWeight, error) {

	// Load the loop-out from DB.
	loopOut, err := p.loopOutFetcher.FetchLoopOutSwap(ctx, swapHash)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to load swap %x from DB: %w",
			swapHash[:6], err)
	}

	contract := loopOut.Contract
	if contract == nil {
		return 0, 0, fmt.Errorf("loop-out %x has nil Contract",
			swapHash[:6])
	}

	// Determine if we can keyspend.
	htlcVersion := utils.GetHtlcScriptVersion(contract.ProtocolVersion)
	canKeyspend := htlcVersion >= swap.HtlcV3

	// Find addInputToEstimator function.
	var addInputToEstimator func(e *input.TxWeightEstimator) error
	if canKeyspend {
		// Assume the server is cooperative and we produce keyspend.
		addInputToEstimator = func(e *input.TxWeightEstimator) error {
			e.AddTaprootKeySpendInput(txscript.SigHashDefault)

			return nil
		}
	} else {
		// Get the HTLC script for our swap.
		htlc, err := utils.GetHtlc(
			swapHash, &contract.SwapContract, p.chainParams,
		)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to get HTLC: %w", err)
		}
		addInputToEstimator = htlc.AddSuccessToEstimator
	}

	// Transaction weight might be important for feeRate, in case of high
	// priority proportional fee, so we accurately assess the size of input.
	// The size of output is almost the same for all types, so use P2TR.
	var destAddr *btcutil.AddressTaproot

	// Get current height.
	height := p.getHeight()
	if height == 0 {
		return 0, 0, fmt.Errorf("got zero best block height")
	}

	// blocksUntilExpiry is the number of blocks until the htlc timeout path
	// opens for the client to sweep.
	blocksUntilExpiry := contract.CltvExpiry - height

	// Find confTarget. If the sweep has expired, use confTarget=1, because
	// confTarget must be positive.
	confTarget := blocksUntilExpiry
	if confTarget <= 0 {
		log.Infof("Swap %x has expired (blocksUntilExpiry=%d), using "+
			"confTarget=1 for it.", swapHash[:6], blocksUntilExpiry)

		confTarget = 1
	}

	feeFactor := float64(1.0)

	// If confTarget is less than or equal to DefaultSweepConfTargetDelta,
	// cap it with urgentSweepConfTarget and apply fee factor.
	if confTarget <= DefaultSweepConfTargetDelta {
		// If confTarget is already <= urgentSweepConfTarget, don't
		// increase it.
		newConfTarget := int32(urgentSweepConfTarget)
		if confTarget < newConfTarget {
			newConfTarget = confTarget
		}

		log.Infof("Swap %x is about to expire (blocksUntilExpiry=%d), "+
			"reducing its confTarget from %d to %d and multiplying"+
			" feerate by %v.", swapHash[:6], blocksUntilExpiry,
			confTarget, newConfTarget, urgentSweepConfTargetFactor)

		confTarget = newConfTarget
		feeFactor = urgentSweepConfTargetFactor
	}

	// Construct the label.
	label := fmt.Sprintf("loopout-sweep-%x", swapHash[:6])

	// Estimate confTarget and feeRate.
	_, feeRate, _, err := p.sweeper.GetSweepFeeDetails(
		ctx, addInputToEstimator, destAddr, confTarget, label,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("fee estimator failed, swapHash=%x, "+
			"confTarget=%d: %w", swapHash[:6], confTarget, err)
	}

	// Multiply feerate by fee factor.
	feeRate = chainfee.SatPerKWeight(float64(feeRate) * feeFactor)

	// Sanity check. Make sure fee rate is not too low.
	const minFeeRate = chainfee.AbsoluteFeePerKwFloor
	if feeRate < minFeeRate {
		log.Infof("Got too low fee rate for swap %x: %v. Increasing "+
			"it to %v.", swapHash[:6], feeRate, minFeeRate)

		feeRate = minFeeRate
	}

	log.Debugf("Estimated for swap %x: feeRate=%s, confTarget=%d.",
		swapHash[:6], feeRate, confTarget)

	return confTarget, feeRate, nil
}
