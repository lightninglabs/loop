package loopin

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// depositSwapHashMigrationID is the identifier for the deposit swap
	// hash migration.
	depositSwapHashMigrationID = "deposit_swap_hash"
)

// MigrateDepositSwapHash will retrieve the comma separated deposit list of
// past and pending swaps and map them to the swap hash in the deposits table.
func MigrateDepositSwapHash(ctx context.Context, db loopdb.SwapStore,
	depositStore *deposit.SqlStore, swapStore *SqlStore) error {

	migrationDone, err := db.HasMigration(
		ctx, depositSwapHashMigrationID,
	)
	if err != nil {
		return fmt.Errorf("unable to check migration status: %w", err)
	}
	if migrationDone {
		log.Infof("Deposit swap hash migration already done, " +
			"skipping")

		return nil
	}

	log.Infof("Starting deposit swap hash migration")
	startTs := time.Now()
	defer func() {
		log.Infof("Finished deposit swap hash migration in %v",
			time.Since(startTs))
	}()

	// First we'll fetch all past loop in swaps from the database.
	swaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctx, AllStates,
	)
	if err != nil {
		return err
	}

	// Now we'll map each deposit of a swap to its respective swap hash.
	depositsToSwapHashes := make(map[deposit.ID]lntypes.Hash)
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

			if _, ok := depositsToSwapHashes[deposit.ID]; !ok {
				depositsToSwapHashes[deposit.ID] = swap.SwapHash
			} else {
				log.Warnf("Duplicate deposit ID %s found for "+
					"outpoint %s, skipping",
					deposit.ID, outpoint)
			}
		}
	}

	log.Infof("Batch-mapping %d deposits to swap hashes",
		len(depositsToSwapHashes))

	err = swapStore.BatchMapDepositsToSwapHashes(ctx, depositsToSwapHashes)
	if err != nil {
		return err
	}

	// Finally mark the migration as done.
	return db.SetMigration(ctx, depositSwapHashMigrationID)
}
