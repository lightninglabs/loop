package loopd

import (
	"context"
	"os"
	"path/filepath"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// migrateBoltdb migrates the boltdb to sqlite.
func migrateBoltdb(ctx context.Context, cfg *Config) error {
	// First get the chain params.
	chainParams, err := lndclient.Network(cfg.Network).ChainParams()
	if err != nil {
		return err
	}

	// First open the bolt db.
	boltdb, err := loopdb.NewBoltSwapStore(cfg.DataDir, chainParams)
	if err != nil {
		return err
	}
	defer boltdb.Close()

	swapDb, _, err := openDatabase(cfg, chainParams)
	if err != nil {
		return err
	}
	defer swapDb.Close()

	// Create a new migrator manager.
	migrator := loopdb.NewMigratorManager(boltdb, swapDb)

	// Run the migration.
	err = migrator.RunMigrations(ctx)
	if err != nil {
		return err
	}

	// If the migration was successful we'll rename the bolt db to
	// loop.db.bk.
	err = os.Rename(
		filepath.Join(cfg.DataDir, "loop.db"),
		filepath.Join(cfg.DataDir, "loop.db.bk"),
	)
	if err != nil {
		return err
	}

	return nil
}

// needSqlMigration checks if the boltdb exists at it's default location
// and returns true if it does.
func needSqlMigration(cfg *Config) bool {
	// First check if the data directory exists.
	if !lnrpc.FileExists(cfg.DataDir) {
		return false
	}

	// Do not migrate if sqlite db already exists. This is to prevent the
	// migration from running multiple times for systems that may restore
	// any deleted files occasionally (reboot, etc).
	sqliteDBPath := filepath.Join(cfg.DataDir, "loop_sqlite.db")
	if lnrpc.FileExists(sqliteDBPath) {
		log.Infof("Found sqlite db at %v, skipping migration",
			sqliteDBPath)

		return false
	}

	// Now we'll check if the bolt db exists.
	if !lnrpc.FileExists(filepath.Join(cfg.DataDir, "loop.db")) {
		return false
	}

	// If both the folder and the bolt db exist, we'll return true.
	return true
}
