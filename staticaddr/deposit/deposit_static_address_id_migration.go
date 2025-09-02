package deposit

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/loop/loopdb"
)

const (
	// depositStaticAddressIDMigrationID is the identifier for the migration
	// that backfills the static_address_id column on existing deposits.
	depositStaticAddressIDMigrationID = "deposit_static_address_id"
)

// MigrateDepositStaticAddressID populates the static_address_id field for all
// existing deposits by assigning the id of the single static address known to
// the client. If no static address exists, the migration is a no-op. If more
// than one static address exists, the migration will fail to avoid ambiguous
// assignment.
func MigrateDepositStaticAddressID(ctx context.Context, db loopdb.SwapStore,
	store *SqlStore) error {

	migrationDone, err := db.HasMigration(
		ctx, depositStaticAddressIDMigrationID,
	)
	if err != nil {
		return fmt.Errorf("unable to check migration status: %w", err)
	}
	if migrationDone {
		log.Infof("Deposit static address id migration already done, " +
			"skipping")

		return nil
	}

	log.Infof("Starting deposit static address id migration")
	startTs := time.Now()
	defer func() {
		log.Infof("Finished deposit static address id migration in %v",
			time.Since(startTs))
	}()

	// Read all static addresses. The address manager logic only allows a
	// single static address to exist for the client.
	addresses, err := store.baseDB.Queries.AllStaticAddresses(ctx)
	if err != nil {
		return fmt.Errorf("unable to fetch static addresses: %w", err)
	}

	switch len(addresses) {
	case 0:
		// Nothing to backfill. Mark as done to avoid re-running.
		log.Infof("No static addresses found, nothing to backfill")

		return db.SetMigration(ctx, depositStaticAddressIDMigrationID)

	case 1:
		// OK.

	default:
		return fmt.Errorf("found %d static addresses, expected 1",
			len(addresses))
	}

	// Backfill all deposits that don't yet have a static address id using
	// the store method.
	err = store.BatchSetStaticAddressID(ctx, addresses[0].ID)
	if err != nil {
		return fmt.Errorf("backfill deposits static_address_id: %w",
			err)
	}

	// Finally, mark the migration as done.
	return db.SetMigration(ctx, depositStaticAddressIDMigrationID)
}
