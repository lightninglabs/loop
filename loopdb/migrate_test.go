//go:build test_migration
// +build test_migration

package loopdb

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

var (
	boltDbFile = "../loopdb-kon"
	addr       = "bc1p4g493qcmzt79r87363fvyvq5sfz58q5gsz74g2c4ejqy5xnpcpesh3yq2y"
	addrBtc, _ = btcutil.DecodeAddress(addr, &chaincfg.MainNetParams)
)

// TestMigrationFromOnDiskBoltdb tests migrating from an on-disk boltdb to an
// sqlite database.
func TestMigrationFromOnDiskBoltdb(t *testing.T) {
	ctxb := context.Background()

	// Open a boltdbStore from the on-disk file.
	boltDb, err := NewBoltSwapStore(boltDbFile, &chaincfg.TestNet3Params)
	require.NoError(t, err)

	// Create a new sqlite store for testing.
	sqlDB := NewTestDB(t)

	migrator := NewMigratorManager(boltDb, sqlDB)

	// Run the migration.
	err = migrator.RunMigrations(ctxb)
	require.NoError(t, err)
}
