package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	sweeppkg "github.com/lightninglabs/loop/sweep"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// greedyAddSweeps selects a batch for the sweeps using the greedy algorithm,
// which minimizes costs, and adds the sweeps to the batch. To accomplish this,
// it first collects fee details about the sweeps being added, about a potential
// new batch composed of these sweeps only, and about all existing batches. It
// skips batches with at least MaxSweepsPerBatch swaps to keep tx standard. Then
// it passes the data to selectBatches() function, which emulates adding the
// sweep to each batch and creating new batch for the sweeps, and calculates the
// costs of each alternative. Based on the estimates of selectBatches(), this
// method adds the sweeps to the batch that results in the least overall fee
// increase, or creates new batch for it. If the sweeps are not accepted by an
// existing batch (may happen because of too distant timeouts), next batch is
// tried in the list returned by selectBatches(). If adding fails or new batch
// creation fails, this method returns an error. If this method fails for any
// reason, the caller falls back to the simple algorithm (method handleSweep).
func (b *Batcher) greedyAddSweeps(ctx context.Context, sweeps []*sweep) error {
	if len(sweeps) == 0 {
		return fmt.Errorf("trying to greedy add an empty sweeps group")
	}

	swap := sweeps[0].swapHash

	// Collect weight and fee rate info about the sweep and new batch.
	sweepFeeDetails, newBatchFeeDetails, err := estimateSweepFeeIncrement(
		sweeps,
	)
	if err != nil {
		return fmt.Errorf("failed to estimate tx weight for "+
			"sweep %x: %w", swap[:6], err)
	}

	// Collect weight and fee rate info about existing batches.
	batches := make([]feeDetails, 0, len(b.batches))
	for _, existingBatch := range b.batches {
		// Enforce MaxSweepsPerBatch. If there are already too many
		// sweeps in the batch, do not add another sweep to prevent the
		// tx from becoming non-standard.
		if len(existingBatch.sweeps) >= MaxSweepsPerBatch {
			continue
		}

		batchFeeDetails, err := estimateBatchWeight(existingBatch)
		if err != nil {
			return fmt.Errorf("failed to estimate tx weight for "+
				"batch %d: %w", existingBatch.id, err)
		}
		batches = append(batches, batchFeeDetails)
	}

	// Run the algorithm. Get batchId of possible batches, sorted from best
	// to worst.
	batchesIds, err := selectBatches(
		batches, sweepFeeDetails, newBatchFeeDetails,
	)
	if err != nil {
		return fmt.Errorf("batch selection algorithm failed for sweep "+
			"%x: %w", swap[:6], err)
	}

	// Try batches, starting with the best.
	for _, batchId := range batchesIds {
		// If the best option is to start new batch, do it.
		if batchId == newBatchSignal {
			return b.spinUpNewBatch(ctx, sweeps)
		}

		// Locate the batch to add the sweeps to.
		bestBatch, has := b.batches[batchId]
		if !has {
			return fmt.Errorf("batch selection algorithm returned "+
				"batch id %d which doesn't exist, for sweep %x",
				batchId, swap[:6])
		}

		// Add the sweeps to the batch.
		accepted, err := bestBatch.addSweeps(ctx, sweeps)
		if err != nil {
			return fmt.Errorf("batch selection algorithm returned "+
				"batch id %d for sweep %x, but adding failed: "+
				"%w", batchId, swap[:6], err)
		}
		if accepted {
			return nil
		}

		debugf("Batch selection algorithm returned batch id %d "+
			"for sweep %x, but acceptance failed.", batchId,
			swap[:6])
	}

	return fmt.Errorf("no batch accepted sweep group %x", swap[:6])
}

// estimateSweepFeeIncrement returns fee details for adding the sweeps to
// a batch and for creating new batch with these sweeps only.
func estimateSweepFeeIncrement(
	sweeps []*sweep) (feeDetails, feeDetails, error) {

	if len(sweeps) == 0 {
		return feeDetails{}, feeDetails{}, fmt.Errorf("estimating an " +
			"empty group of sweeps")
	}

	// Create a fake batch with the sweeps.
	batch := &batch{
		rbfCache: rbfCache{
			FeeRate: sweeps[0].minFeeRate,
		},
		sweeps: make(map[wire.OutPoint]sweep, len(sweeps)),
	}
	for _, s := range sweeps {
		batch.sweeps[s.outpoint] = *s
		batch.rbfCache.FeeRate = max(
			batch.rbfCache.FeeRate, s.minFeeRate,
		)
	}

	// Estimate new batch.
	fd1, err := estimateBatchWeight(batch)
	if err != nil {
		return feeDetails{}, feeDetails{}, err
	}

	// Add the same sweeps again with different outpoints to measure weight
	// increments.
	for _, s := range sweeps {
		dummy := s.outpoint
		dummy.Hash[0]++
		if _, has := batch.sweeps[dummy]; has {
			return feeDetails{}, feeDetails{}, fmt.Errorf("dummy "+
				"outpoint %s is present in the batch", dummy)
		}
		batch.sweeps[dummy] = *s
	}

	// Estimate weight of a batch with two sweeps.
	fd2, err := estimateBatchWeight(batch)
	if err != nil {
		return feeDetails{}, feeDetails{}, err
	}

	// Create feeDetails for sweep.
	sweepFeeDetails := feeDetails{
		FeeRate:        batch.rbfCache.FeeRate,
		IsExternalAddr: sweeps[0].isExternalAddr,

		// Calculate sweep weight as a difference.
		Weight: fd2.Weight - fd1.Weight,
	}

	return sweepFeeDetails, fd1, nil
}

// estimateBatchWeight estimates batch weight and returns its fee details.
func estimateBatchWeight(batch *batch) (feeDetails, error) {
	// Make sure the batch is not empty.
	if len(batch.sweeps) == 0 {
		return feeDetails{}, errors.New("empty batch")
	}

	// Make sure fee rate is valid.
	if batch.rbfCache.FeeRate < chainfee.AbsoluteFeePerKwFloor {
		return feeDetails{}, fmt.Errorf("feeRate is too low: %v",
			batch.rbfCache.FeeRate)
	}

	// Find some sweep of the batch. It is used if there is just one sweep.
	var theSweep sweep
	for _, sweep := range batch.sweeps {
		theSweep = sweep
		break
	}

	// Find sweep destination address (type) for weight estimations.
	var destAddr btcutil.Address
	if theSweep.isExternalAddr {
		if theSweep.destAddr == nil {
			return feeDetails{}, errors.New("isExternalAddr=true," +
				" but destAddr is nil")
		}
		destAddr = theSweep.destAddr
	} else {
		// Assume it is taproot by default.
		destAddr = (*btcutil.AddressTaproot)(nil)
	}

	// Make a weight estimator.
	var weight input.TxWeightEstimator

	// Add output weight to the estimator.
	err := sweeppkg.AddOutputEstimate(&weight, destAddr)
	if err != nil {
		return feeDetails{}, fmt.Errorf("sweep.AddOutputEstimate: %w",
			err)
	}

	// Add change output weights. Change outputs with identical pkscript
	// will be consolidated into a single output.
	changeOutputs := make(map[string]struct{})
	for _, s := range batch.sweeps {
		if s.change == nil {
			continue
		}

		pkScriptString := string(s.change.PkScript)
		if _, has := changeOutputs[pkScriptString]; has {
			continue
		}

		weight.AddOutput(s.change.PkScript)
		changeOutputs[pkScriptString] = struct{}{}
	}

	// Add inputs.
	for _, sweep := range batch.sweeps {
		if sweep.nonCoopHint || sweep.coopFailed {
			err = sweep.htlcSuccessEstimator(&weight)
			if err != nil {
				return feeDetails{}, fmt.Errorf(
					"htlcSuccessEstimator failed: %w", err,
				)
			}
		} else {
			weight.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}
	}

	return feeDetails{
		BatchId:        batch.id,
		FeeRate:        batch.rbfCache.FeeRate,
		Weight:         weight.Weight(),
		IsExternalAddr: theSweep.isExternalAddr,
	}, nil
}

// newBatchSignal is the value that indicates a new batch. It is returned by
// selectBatches to encode new batch creation.
const newBatchSignal = -1

// feeDetails is either a batch or a sweep and it holds data important for
// selection of a batch to add the sweep to (or new batch creation).
type feeDetails struct {
	BatchId        int32
	FeeRate        chainfee.SatPerKWeight
	Weight         lntypes.WeightUnit
	IsExternalAddr bool
}

// fee returns fee of onchain transaction representing this instance.
func (e feeDetails) fee() btcutil.Amount {
	return e.FeeRate.FeeForWeight(e.Weight)
}

// combine returns new feeDetails, combining properties.
func (e1 feeDetails) combine(e2 feeDetails) feeDetails {
	// The fee rate is max of two fee rates.
	feeRate := e1.FeeRate
	if feeRate < e2.FeeRate {
		feeRate = e2.FeeRate
	}

	return feeDetails{
		FeeRate:        feeRate,
		Weight:         e1.Weight + e2.Weight,
		IsExternalAddr: e1.IsExternalAddr || e2.IsExternalAddr,
	}
}

// selectBatches returns the list of id of batches sorted from best to worst.
// Creation a new batch is encoded as newBatchSignal. For each batch its fee
// rate and a weight is provided. Also, a hint is provided to signal which
// spending path will be used by the batch.
//
// The same data is also provided for the sweep (or sweeps) for which we are
// selecting a batch to add. In case of the sweep weights are weight deltas
// resulted from adding the sweep. Finally, the same data is provided for new
// batch having this sweep(s) only.
//
// The algorithm compares costs of adding the sweep to each existing batch, and
// costs of new batch creation for this sweep and returns BatchId of the winning
// batch. If the best option is to create a new batch, return newBatchSignal.
//
// Each fee details has also IsExternalAddr flag. There is a rule that sweeps
// having flag IsExternalAddr must go in individual batches. Cooperative
// spending may only be available for some sweeps supporting it, not for all.
func selectBatches(batches []feeDetails,
	added, newBatch feeDetails) ([]int32, error) {

	// If the sweep has IsExternalAddr flag, the sweep can't be added to
	// a batch, so create new batch for it.
	if added.IsExternalAddr {
		return []int32{newBatchSignal}, nil
	}

	// alternative holds batch ID and its cost.
	type alternative struct {
		batchId int32
		cost    btcutil.Amount
	}

	// Create the list of possible actions and their costs.
	alternatives := make([]alternative, 0, len(batches)+1)

	// Track the best batch to add a sweep to. The default case is new batch
	// creation with this sweep only in it. The cost is its full fee.
	alternatives = append(alternatives, alternative{
		batchId: newBatchSignal,
		cost:    newBatch.fee(),
	})

	// Try to add the sweep to every batch, calculate the costs and
	// find the batch adding to which results in minimum costs.
	for _, batch := range batches {
		// If the batch has IsExternalAddr flag, the sweep can't be
		// added to it, so skip the batch.
		if batch.IsExternalAddr {
			continue
		}

		// Add the sweep to the batch virtually.
		combinedBatch := batch.combine(added)

		// The cost is the fee increase.
		cost := combinedBatch.fee() - batch.fee()

		// The cost must be positive, because we added a sweep.
		if cost <= 0 {
			return nil, fmt.Errorf("got non-positive cost of "+
				"adding sweep to batch %d: %d", batch.BatchId,
				cost)
		}

		// Track the best batch, according to the costs.
		alternatives = append(alternatives, alternative{
			batchId: batch.BatchId,
			cost:    cost,
		})
	}

	// Sort the alternatives by cost. The lower the cost, the better.
	sort.Slice(alternatives, func(i, j int) bool {
		return alternatives[i].cost < alternatives[j].cost
	})

	// Collect batches IDs.
	batchesIds := make([]int32, len(alternatives))
	for i, alternative := range alternatives {
		batchesIds[i] = alternative.batchId
	}

	return batchesIds, nil
}
