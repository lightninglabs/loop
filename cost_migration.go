package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// costMigrationID is the identifier for the cost migration.
	costMigrationID = "cost_migration"
)

// CalculateLoopOutCost calculates the total cost of a loop out swap. It will
// correctly account for the on-chain and off-chain fees that were paid and
// make sure that all costs are positive.
func CalculateLoopOutCost(params *chaincfg.Params, loopOutSwap *loopdb.LoopOut,
	paymentFees map[lntypes.Hash]lnwire.MilliSatoshi) (loopdb.SwapCost,
	error) {

	// First, make sure that this swap is actually finished.
	if loopOutSwap.State().State.IsPending() {
		return loopdb.SwapCost{}, fmt.Errorf("swap is not yet finished")
	}

	// We first need to decode the prepay invoice to get the prepay hash and
	// the prepay amount.
	_, _, hash, prepayAmount, err := swap.DecodeInvoice(
		params, loopOutSwap.Contract.PrepayInvoice,
	)
	if err != nil {
		return loopdb.SwapCost{}, fmt.Errorf("unable to decode the "+
			"prepay invoice: %v", err)
	}

	// The swap hash is given and we don't need to get it from the
	// swap invoice, however we'll decode it anyway to get the invoice
	// amount that was paid in case we don't have the payment anymore.
	_, _, swapHash, swapPaymentAmount, err := swap.DecodeInvoice(
		params, loopOutSwap.Contract.SwapInvoice,
	)
	if err != nil {
		return loopdb.SwapCost{}, fmt.Errorf("unable to decode the "+
			"swap invoice: %v", err)
	}

	var (
		cost                 loopdb.SwapCost
		swapPaid, prepayPaid bool
	)

	// Now that we have the prepay and swap amount, we can calculate the
	// total cost of the swap. Note that we only need to account for the
	// server cost in case the swap was successful or if the sweep timed
	// out. Otherwise the server didn't pull the off-chain htlc nor the
	// prepay.
	switch loopOutSwap.State().State {
	case loopdb.StateSuccess:
		cost.Server = swapPaymentAmount + prepayAmount -
			loopOutSwap.Contract.AmountRequested

		swapPaid = true
		prepayPaid = true

	case loopdb.StateFailSweepTimeout:
		cost.Server = prepayAmount

		prepayPaid = true

	default:
		cost.Server = 0
	}

	// Now attempt to look up the actual payments so we can calculate the
	// total routing costs.
	prepayPaymentFee, ok := paymentFees[hash]
	if prepayPaid && ok {
		cost.Offchain += prepayPaymentFee.ToSatoshis()
	} else {
		log.Debugf("Prepay payment %s is missing, won't account for "+
			"routing fees", hash)
	}

	swapPaymentFee, ok := paymentFees[swapHash]
	if swapPaid && ok {
		cost.Offchain += swapPaymentFee.ToSatoshis()
	} else {
		log.Debugf("Swap payment %s is missing, won't account for "+
			"routing fees", swapHash)
	}

	// For the on-chain cost, just make sure that the cost is positive.
	cost.Onchain = loopOutSwap.State().Cost.Onchain
	if cost.Onchain < 0 {
		cost.Onchain *= -1
	}

	return cost, nil
}

// MigrateLoopOutCosts will calculate the correct cost for all loop out swaps
// and override the cost values of the last update in the database.
func MigrateLoopOutCosts(ctx context.Context, lnd lndclient.LndServices,
	paymentBatchSize int, db loopdb.SwapStore) error {

	migrationDone, err := db.HasMigration(ctx, costMigrationID)
	if err != nil {
		return err
	}
	if migrationDone {
		log.Infof("Cost cleanup migration already done, skipping")

		return nil
	}

	log.Infof("Starting cost cleanup migration")
	startTs := time.Now()
	defer func() {
		log.Infof("Finished cost cleanup migration in %v",
			time.Since(startTs))
	}()

	// First we'll fetch all loop out swaps from the database.
	loopOutSwaps, err := db.FetchLoopOutSwaps(ctx)
	if err != nil {
		return err
	}

	// Gather payment fees to a map for easier lookup.
	paymentFees := make(map[lntypes.Hash]lnwire.MilliSatoshi)
	offset := uint64(0)

	for {
		payments, err := lnd.Client.ListPayments(
			ctx, lndclient.ListPaymentsRequest{
				Offset:      offset,
				MaxPayments: uint64(paymentBatchSize),
			},
		)
		if err != nil {
			return err
		}

		if len(payments.Payments) == 0 {
			break
		}

		for _, payment := range payments.Payments {
			paymentFees[payment.Hash] = payment.Fee
		}

		offset = payments.LastIndexOffset + 1
	}

	// Now we'll calculate the cost for each swap and finally update the
	// costs in the database.
	updatedCosts := make(map[lntypes.Hash]loopdb.SwapCost)
	for _, loopOutSwap := range loopOutSwaps {
		if loopOutSwap.State().State.IsPending() {
			continue
		}

		cost, err := CalculateLoopOutCost(
			lnd.ChainParams, loopOutSwap, paymentFees,
		)
		if err != nil {
			// We don't want to fail loopd because of any old swap
			// that we're unable to calculate the cost for. We'll
			// warn though so that we can investigate further.
			log.Warnf("Unable to calculate cost for swap %v: %v",
				loopOutSwap.Hash, err)

			continue
		}

		_, ok := updatedCosts[loopOutSwap.Hash]
		if ok {
			return fmt.Errorf("found a duplicate swap %v while "+
				"updating costs", loopOutSwap.Hash)
		}

		updatedCosts[loopOutSwap.Hash] = cost
	}

	log.Infof("Updating costs for %d loop out swaps", len(updatedCosts))
	err = db.BatchUpdateLoopOutSwapCosts(ctx, updatedCosts)
	if err != nil {
		return err
	}

	// Finally, mark the migration as done.
	return db.SetMigration(ctx, costMigrationID)
}
