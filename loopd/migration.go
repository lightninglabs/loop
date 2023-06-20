package loopd

import (
	"context"
	"fmt"
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

	var db loopdb.SwapStore
	switch cfg.DatabaseBackend {
	case DatabaseBackendSqlite:
		log.Infof("Opening sqlite3 database at: %v",
			cfg.Sqlite.DatabaseFileName)
		db, err = loopdb.NewSqliteStore(
			cfg.Sqlite, chainParams,
		)

	case DatabaseBackendPostgres:
		log.Infof("Opening postgres database at: %v",
			cfg.Postgres.DSN(true))
		db, err = loopdb.NewPostgresStore(
			cfg.Postgres, chainParams,
		)

	default:
		return fmt.Errorf("unknown database backend: %s",
			cfg.DatabaseBackend)
	}
	if err != nil {
		return fmt.Errorf("unable to open database: %v", err)
	}

	defer db.Close()

	// Create a new migrator manager.
	migrator := loopdb.NewMigratorManager(boltdb, db)

	// Run the migration.
	err = migrator.RunMigrations(ctx)
	if err != nil {
		return err
	}

	// If the migration was successfull we'll rename the bolt db to
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

	// Now we'll check if the bolt db exists.
	if !lnrpc.FileExists(filepath.Join(cfg.DataDir, "loop.db")) {
		return false
	}

	// If both the folder and the bolt db exist, we'll return true.
	return true
}
