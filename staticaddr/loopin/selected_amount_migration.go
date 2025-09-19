package loopin

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// selectedAmountMigrationID is the identifier for the selected swap
	// amount migration.
	selectedAmountMigrationID = "selected_amount"
)

// MigrateSelectedSwapAmount will update the selected swap amount of past swaps
// with the sum of the values of the deposits they swapped.
func MigrateSelectedSwapAmount(ctx context.Context, db loopdb.SwapStore,
	depositStore *deposit.SqlStore, swapStore *SqlStore) error {

	migrationDone, err := db.HasMigration(ctx, selectedAmountMigrationID)
	if err != nil {
		return fmt.Errorf("unable to check migration status: %w", err)
	}
	if migrationDone {
		log.Infof("Selected swap amount migration already done, " +
			"skipping")

		return nil
	}

	log.Infof("Starting swap amount migration")
	startTs := time.Now()
	defer func() {
		log.Infof("Finished swap amount migration in %v",
			time.Since(startTs))
	}()

	// First we'll fetch all loop out swaps from the database.
	swaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctx, FinalStates,
	)
	if err != nil {
		return err
	}

	// Now we'll calculate the cost for each swap and finally update the
	// costs in the database.
	// TODO(hieblmi): normalize swap hash and deposit ids.
	updateAmounts := make(map[lntypes.Hash]btcutil.Amount)
	for _, swap := range swaps {
		for _, outpoint := range swap.DepositOutpoints {
			deposit, err := depositStore.DepositForOutpoint(
				ctx, outpoint,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch deposit "+
					"for outpoint %s: %w", outpoint, err)
			}
			if deposit == nil {
				return fmt.Errorf("deposit for outpoint %s "+
					"not found", outpoint)
			}

			// Set the selected amount to the value of the deposit.
			updateAmounts[swap.SwapHash] += deposit.Value
		}
	}

	log.Infof("Updating selected swap amounts for %d loop in swaps",
		len(updateAmounts))

	err = swapStore.BatchUpdateSelectedSwapAmounts(ctx, updateAmounts)
	if err != nil {
		return err
	}

	// Finally mark the migration as done.
	return db.SetMigration(ctx, selectedAmountMigrationID)
}
